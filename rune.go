package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/veandco/go-sdl2/sdl"
	"github.com/veandco/go-sdl2/ttf"
)

//go:embed JetBrainsMonoNL-Regular.ttf
var fontData []byte

const (
	programName         = "Rune"
	initialWindowWidth  = 800
	initialWindowHeight = 600
	fontSize            = 16
	lineHeight          = 20
	tabWidth            = 4
	gutterWidth         = 60
	defaultBrowserWidth = 250
	textCacheLimit      = 2048
	tabBarHeight        = 24
)

func clamp(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type Position struct {
	x, y int
}

type UndoOp struct {
	deleteAt     int
	deleted      []rune
	insertAt     int
	inserted     []rune
	cursorBefore Position
	cursorAfter  Position
}

type FileState struct {
	buffer           *PieceTable
	cursor           Position
	scroll           Position
	undoStack        []UndoOp
	redoStack        []UndoOp
	savedAtUndoDepth int
	dirty            bool
	untitled         bool
}

type EditorConfig struct {
	Ignore         []string `json:"ignore"`
	AutosaveMillis int      `json:"autosave_ms"`
	BrowserWidth   int      `json:"browser_width"`
}

type SessionState struct {
	Root        string   `json:"root"`
	OpenFiles   []string `json:"open_files"`
	CurrentFile string   `json:"current_file"`
}

type OverlayMode int

const (
	overlayNone OverlayMode = iota
	overlayCommand
	overlayOpen
	overlayCreate
	overlayRename
	overlayDelete
	overlayGoto
	overlayFilter
	overlaySaveAs
	overlayReplace
)

type FileNode struct {
	name     string
	path     string
	isDir    bool
	expanded bool
	children []*FileNode
	depth    int
}

type PieceSource int

const (
	pieceOriginal PieceSource = iota
	pieceAdd
)

type Piece struct {
	source PieceSource
	start  int
	length int
}

type PieceTable struct {
	original     []rune
	add          []rune
	pieces       []Piece
	pieceOffsets []int
	length       int
	lineStarts   []int
}

type CachedText struct {
	texture  *sdl.Texture
	w        int32
	h        int32
	lastUsed uint64
}

func NewPieceTable(text string) *PieceTable {
	original := []rune(text)
	p := &PieceTable{
		original:   original,
		length:     len(original),
		lineStarts: []int{0},
	}
	if len(original) > 0 {
		p.pieces = []Piece{{source: pieceOriginal, start: 0, length: len(original)}}
		p.pieceOffsets = []int{0}
	}
	for i, r := range original {
		if r == '\n' {
			p.lineStarts = append(p.lineStarts, i+1)
		}
	}
	return p
}

func (p *PieceTable) Clone() *PieceTable {
	if p == nil {
		return NewPieceTable("")
	}
	clone := &PieceTable{
		original:     append([]rune(nil), p.original...),
		add:          append([]rune(nil), p.add...),
		pieces:       append([]Piece(nil), p.pieces...),
		pieceOffsets: append([]int(nil), p.pieceOffsets...),
		length:       p.length,
		lineStarts:   append([]int(nil), p.lineStarts...),
	}
	return clone
}

func (p *PieceTable) Len() int {
	if p == nil {
		return 0
	}
	return p.length
}

func (p *PieceTable) pieceRunes(piece Piece) []rune {
	if piece.source == pieceOriginal {
		return p.original[piece.start : piece.start+piece.length]
	}
	return p.add[piece.start : piece.start+piece.length]
}

func (p *PieceTable) String() string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	b.Grow(p.Len())
	for _, piece := range p.pieces {
		b.WriteString(string(p.pieceRunes(piece)))
	}
	return b.String()
}

func (p *PieceTable) Slice(offset, length int) []rune {
	if p == nil || length <= 0 {
		return nil
	}
	offset = clamp(offset, 0, p.length)
	if offset+length > p.length {
		length = p.length - offset
	}
	if length <= 0 {
		return nil
	}
	result := make([]rune, 0, length)
	end := offset + length
	firstPiece := sort.Search(len(p.pieces), func(i int) bool {
		return p.pieceOffsets[i]+p.pieces[i].length > offset
	})
	for i := firstPiece; i < len(p.pieces); i++ {
		piece := p.pieces[i]
		pieceOffset := p.pieceOffsets[i]
		if pieceOffset >= end {
			break
		}
		startInPiece := max(0, offset-pieceOffset)
		endInPiece := min(piece.length, end-pieceOffset)
		result = append(result, p.pieceRunes(piece)[startInPiece:endInPiece]...)
	}
	return result
}

func (p *PieceTable) LineCount() int {
	if p == nil {
		return 1
	}
	return len(p.lineStarts)
}

func (p *PieceTable) Line(line int) []rune {
	if p == nil || line < 0 || line >= len(p.lineStarts) {
		return nil
	}
	start := p.lineStarts[line]
	end := p.length
	if line+1 < len(p.lineStarts) {
		end = p.lineStarts[line+1] - 1
	}
	return p.Slice(start, end-start)
}

func (p *PieceTable) LineLength(line int) int {
	if p == nil || line < 0 || line >= len(p.lineStarts) {
		return 0
	}
	end := p.length
	if line+1 < len(p.lineStarts) {
		end = p.lineStarts[line+1] - 1
	}
	return end - p.lineStarts[line]
}

func (p *PieceTable) PositionOffset(pos Position) int {
	if p == nil {
		return 0
	}
	pos.y = clamp(pos.y, 0, p.LineCount()-1)
	lineStart := p.lineStarts[pos.y]
	lineLength := p.LineLength(pos.y)
	return lineStart + clamp(pos.x, 0, lineLength)
}

func (p *PieceTable) TextRange(start, end Position) []rune {
	startOffset := p.PositionOffset(start)
	endOffset := p.PositionOffset(end)
	if endOffset < startOffset {
		startOffset, endOffset = endOffset, startOffset
	}
	return p.Slice(startOffset, endOffset-startOffset)
}

func (p *PieceTable) splitAt(offset int) int {
	offset = clamp(offset, 0, p.Len())
	if offset == 0 {
		return 0
	}
	walked := 0
	for i, piece := range p.pieces {
		next := walked + piece.length
		if offset == next {
			return i + 1
		}
		if offset < next {
			leftLen := offset - walked
			rightLen := piece.length - leftLen
			left := Piece{source: piece.source, start: piece.start, length: leftLen}
			right := Piece{source: piece.source, start: piece.start + leftLen, length: rightLen}
			newPieces := make([]Piece, 0, len(p.pieces)+1)
			newPieces = append(newPieces, p.pieces[:i]...)
			if left.length > 0 {
				newPieces = append(newPieces, left)
			}
			if right.length > 0 {
				newPieces = append(newPieces, right)
			}
			newPieces = append(newPieces, p.pieces[i+1:]...)
			p.pieces = newPieces
			if left.length == 0 {
				return i
			}
			return i + 1
		}
		walked = next
	}
	return len(p.pieces)
}

func (p *PieceTable) Insert(offset int, text []rune) {
	if p == nil || len(text) == 0 {
		return
	}
	offset = clamp(offset, 0, p.Len())
	addStart := len(p.add)
	p.add = append(p.add, text...)
	insertPiece := Piece{source: pieceAdd, start: addStart, length: len(text)}
	idx := p.splitAt(offset)
	p.pieces = append(p.pieces, Piece{})
	copy(p.pieces[idx+1:], p.pieces[idx:])
	p.pieces[idx] = insertPiece
	for i := range p.lineStarts {
		if p.lineStarts[i] > offset {
			p.lineStarts[i] += len(text)
		}
	}
	newStarts := make([]int, 0)
	for i, r := range text {
		if r == '\n' {
			newStarts = append(newStarts, offset+i+1)
		}
	}
	if len(newStarts) > 0 {
		insertAt := sort.SearchInts(p.lineStarts, newStarts[0])
		p.lineStarts = append(p.lineStarts, make([]int, len(newStarts))...)
		copy(p.lineStarts[insertAt+len(newStarts):], p.lineStarts[insertAt:len(p.lineStarts)-len(newStarts)])
		copy(p.lineStarts[insertAt:], newStarts)
	}
	p.length += len(text)
	p.mergeAdjacent()
}

func (p *PieceTable) Delete(offset, length int) {
	if p == nil || length <= 0 {
		return
	}
	total := p.Len()
	offset = clamp(offset, 0, total)
	if offset >= total {
		return
	}
	if offset+length > total {
		length = total - offset
	}
	startIdx := p.splitAt(offset)
	endIdx := p.splitAt(offset + length)
	p.pieces = append(p.pieces[:startIdx], p.pieces[endIdx:]...)
	end := offset + length
	updatedStarts := p.lineStarts[:0]
	for _, start := range p.lineStarts {
		switch {
		case start > offset && start <= end:
			continue
		case start > end:
			start -= length
		}
		updatedStarts = append(updatedStarts, start)
	}
	p.lineStarts = updatedStarts
	p.length -= length
	p.mergeAdjacent()
}

func (p *PieceTable) mergeAdjacent() {
	if len(p.pieces) < 2 {
		p.rebuildPieceOffsets()
		return
	}
	merged := p.pieces[:0]
	for _, piece := range p.pieces {
		if piece.length == 0 {
			continue
		}
		if len(merged) > 0 {
			last := &merged[len(merged)-1]
			if last.source == piece.source && last.start+last.length == piece.start {
				last.length += piece.length
				continue
			}
		}
		merged = append(merged, piece)
	}
	p.pieces = merged
	p.rebuildPieceOffsets()
}

func (p *PieceTable) rebuildPieceOffsets() {
	p.pieceOffsets = make([]int, len(p.pieces))
	offset := 0
	for i, piece := range p.pieces {
		p.pieceOffsets[i] = offset
		offset += piece.length
	}
}

func cloneUndoStack(stack []UndoOp) []UndoOp {
	if stack == nil {
		return nil
	}
	clone := make([]UndoOp, len(stack))
	for i, op := range stack {
		clone[i] = op
		clone[i].deleted = append([]rune(nil), op.deleted...)
		clone[i].inserted = append([]rune(nil), op.inserted...)
	}
	return clone
}

type Editor struct {
	buffer                   *PieceTable
	cursorX                  int
	cursorY                  int
	idealCursorX             int // Ideal pixel X position for vertical movement
	scrollOffsetY            int
	scrollOffsetX            int
	windowWidth              int32
	windowHeight             int32
	selectionStart           *Position
	selectionEnd             *Position
	undoStack                []UndoOp
	redoStack                []UndoOp
	mouseDown                bool
	scrollbarDragging        bool
	fileBrowserResizing      bool
	rootDir                  string
	currentFile              string
	untitled                 bool
	untitledName             string
	untitledCounter          int
	openFiles                []string
	fileStates               map[string]*FileState
	isDirty                  bool
	savedAtUndoDepth         int // Undo stack depth when file was saved (-1 = unreachable)
	fileTree                 *FileNode
	nodeByPath               map[string]*FileNode
	flatFileList             []*FileNode
	fileBrowserScroll        int
	fileBrowserScrollX       int
	fileBrowserWidth         int
	fileBrowserMaxWidth      int
	fileBrowserMaxWidthDirty bool
	window                   *sdl.Window
	renderer                 *sdl.Renderer
	font                     *ttf.Font
	textCache                map[string]*CachedText
	textCacheClock           uint64
	charWidth                int
	running                  bool
	maxLineWidth             int         // Cached maximum line width in pixels
	maxLineWidthDirty        bool        // Whether maxLineWidth needs recalculation
	lastClickTime            uint32      // Timestamp of last mouse click
	lastClickCursorX         int         // Character X position of last click
	lastClickCursorY         int         // Character Y position (line) of last click
	clickCount               int         // Number of consecutive clicks (1, 2, 3)
	arrowCursor              *sdl.Cursor // Default arrow cursor
	ibeamCursor              *sdl.Cursor // Text/I-beam cursor
	searchActive             bool        // Whether search box is currently active
	searchMatches            []Position  // All match positions
	searchMatchLens          []int
	currentMatchIndex        int        // Index of currently highlighted match
	savedEditorState         *FileState // Saved editor state when search is active
	searchCaseSensitive      bool
	searchWholeWord          bool
	searchRegex              bool
	searchReplace            string
	overlayActive            bool
	overlayMode              OverlayMode
	overlayTitle             string
	overlayText              string
	fileFilter               string
	ignoreNames              map[string]bool
	stateDir                 string
	autosaveInterval         time.Duration
	lastAutosave             time.Time
}

func NewEditor(rootPath string) (*Editor, error) {
	if err := sdl.Init(sdl.INIT_VIDEO); err != nil {
		return nil, err
	}

	if err := ttf.Init(); err != nil {
		return nil, err
	}

	window, err := sdl.CreateWindow(
		programName,
		sdl.WINDOWPOS_UNDEFINED,
		sdl.WINDOWPOS_UNDEFINED,
		initialWindowWidth,
		initialWindowHeight,
		sdl.WINDOW_SHOWN|sdl.WINDOW_RESIZABLE,
	)
	if err != nil {
		return nil, err
	}

	renderer, err := sdl.CreateRenderer(window, -1, sdl.RENDERER_ACCELERATED)
	if err != nil {
		return nil, err
	}

	// Load font from embedded bytes
	fontRW, err := sdl.RWFromMem(fontData)
	if err != nil {
		return nil, fmt.Errorf("failed to create RW from font data: %v", err)
	}
	font, err := ttf.OpenFontRW(fontRW, 1, fontSize)
	if err != nil {
		return nil, fmt.Errorf("failed to load font: %v", err)
	}

	// Use provided path or current working directory
	targetDir := rootPath
	if targetDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			targetDir = "."
		} else {
			targetDir = cwd
		}
	}

	// Convert to absolute path
	absPath, err := filepath.Abs(targetDir)
	if err != nil {
		absPath = targetDir
	}

	// Verify the path exists and is a directory
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("path does not exist: %s", absPath)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("path is not a directory: %s", absPath)
	}
	stateDir := filepath.Join(absPath, ".rune")
	_ = os.MkdirAll(filepath.Join(stateDir, "autosave"), 0755)
	config := loadEditorConfig(absPath)
	ignoreNames := make(map[string]bool)
	for _, name := range []string{".git", ".rune", "node_modules", "dist", "vendor"} {
		ignoreNames[name] = true
	}
	for _, name := range config.Ignore {
		if name != "" {
			ignoreNames[name] = true
		}
	}
	browserWidth := config.BrowserWidth
	if browserWidth <= 0 {
		browserWidth = defaultBrowserWidth
	}
	browserWidth = clamp(browserWidth, 120, 600)
	autosaveMillis := config.AutosaveMillis
	if autosaveMillis <= 0 {
		autosaveMillis = 3000
	}

	// Create system cursors
	arrowCursor := sdl.CreateSystemCursor(sdl.SYSTEM_CURSOR_ARROW)
	ibeamCursor := sdl.CreateSystemCursor(sdl.SYSTEM_CURSOR_IBEAM)

	editor := &Editor{
		buffer:                   NewPieceTable(""),
		cursorX:                  0,
		cursorY:                  0,
		idealCursorX:             -1,
		windowWidth:              initialWindowWidth,
		windowHeight:             initialWindowHeight,
		rootDir:                  absPath,
		untitledCounter:          1,
		fileStates:               make(map[string]*FileState),
		nodeByPath:               make(map[string]*FileNode),
		isDirty:                  false,
		savedAtUndoDepth:         0,
		fileBrowserWidth:         browserWidth,
		window:                   window,
		renderer:                 renderer,
		font:                     font,
		textCache:                make(map[string]*CachedText),
		charWidth:                1,
		running:                  true,
		maxLineWidth:             0,
		maxLineWidthDirty:        true,
		fileBrowserMaxWidthDirty: true,
		arrowCursor:              arrowCursor,
		ibeamCursor:              ibeamCursor,
		ignoreNames:              ignoreNames,
		stateDir:                 stateDir,
		autosaveInterval:         time.Duration(autosaveMillis) * time.Millisecond,
	}
	if w, _, err := font.SizeUTF8("M"); err == nil && w > 0 {
		editor.charWidth = w
	}

	// Build file tree
	editor.buildFileTree()
	editor.restoreSession()

	return editor, nil
}

func loadEditorConfig(root string) EditorConfig {
	config := EditorConfig{}
	for _, path := range []string{
		filepath.Join(root, ".rune", "config.json"),
		filepath.Join(root, ".rune.json"),
	} {
		data, err := os.ReadFile(path)
		if err == nil {
			_ = json.Unmarshal(data, &config)
			return config
		}
	}
	return config
}

func (e *Editor) buildFileTree() {
	e.nodeByPath = make(map[string]*FileNode)
	e.fileTree = &FileNode{
		name:     filepath.Base(e.rootDir),
		path:     e.rootDir,
		isDir:    true,
		expanded: true,
		depth:    0,
	}
	e.nodeByPath[e.fileTree.path] = e.fileTree
	e.readDirectory(e.fileTree)
	e.flattenTree()
}

func (e *Editor) readDirectory(node *FileNode) {
	if !node.isDir {
		return
	}

	entries, err := os.ReadDir(node.path)
	if err != nil {
		return
	}

	// Sort: directories first, then files alphabetically
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		// Skip hidden files
		if strings.HasPrefix(entry.Name(), ".") || e.ignoreNames[entry.Name()] {
			continue
		}

		child := &FileNode{
			name:     entry.Name(),
			path:     filepath.Join(node.path, entry.Name()),
			isDir:    entry.IsDir(),
			expanded: false,
			depth:    node.depth + 1,
		}

		node.children = append(node.children, child)
		e.nodeByPath[child.path] = child

		// Recursively read subdirectories if expanded
		if child.isDir && child.expanded {
			e.readDirectory(child)
		}
	}
}

func (e *Editor) flattenTree() {
	e.flatFileList = nil
	e.flattenNode(e.fileTree)
	e.invalidateFileBrowserWidth()
}

func (e *Editor) flattenNode(node *FileNode) {
	if node == nil {
		return
	}

	if e.fileFilter != "" && !e.nodeMatchesFilter(node) {
		return
	}
	e.flatFileList = append(e.flatFileList, node)

	if node.isDir && node.expanded {
		for _, child := range node.children {
			e.flattenNode(child)
		}
	}
}

func (e *Editor) nodeMatchesFilter(node *FileNode) bool {
	filter := strings.ToLower(e.fileFilter)
	if strings.Contains(strings.ToLower(node.name), filter) {
		return true
	}
	if node.isDir {
		for _, child := range node.children {
			if e.nodeMatchesFilter(child) {
				return true
			}
		}
	}
	return false
}

func (e *Editor) fileDirty(path string) bool {
	if path == e.currentFile {
		return e.isDirty
	}
	state, ok := e.fileStates[path]
	return ok && state.dirty
}

// findNode recursively searches for a node with the given path
func (e *Editor) findNode(node *FileNode, path string) *FileNode {
	if found, ok := e.nodeByPath[path]; ok {
		return found
	}
	if node == nil {
		return nil
	}

	if node.path == path {
		return node
	}

	for _, child := range node.children {
		if found := e.findNode(child, path); found != nil {
			return found
		}
	}

	return nil
}

// getHighlightedNode returns the node that should be highlighted in the file browser
// based on the currently open file and directory expansion state
func (e *Editor) getHighlightedNode() *FileNode {
	if e.currentFile == "" {
		return nil
	}

	// Find the node for the current file
	targetNode := e.findNode(e.fileTree, e.currentFile)
	if targetNode == nil {
		return nil
	}

	// Check if this node is visible in the flat list
	for _, node := range e.flatFileList {
		if node.path == targetNode.path {
			// Node is visible, return it
			return targetNode
		}
	}

	// Node is not visible, find the closest visible parent
	// Walk up from the target path
	currentPath := filepath.Dir(e.currentFile)
	for currentPath != e.rootDir && currentPath != "/" && currentPath != "." {
		node := e.findNode(e.fileTree, currentPath)
		if node != nil {
			// Check if this parent is visible
			for _, flatNode := range e.flatFileList {
				if flatNode.path == node.path {
					return node
				}
			}
		}
		currentPath = filepath.Dir(currentPath)
	}

	// If we get here, return the root
	return e.fileTree
}

// expandTabsForDisplay converts tabs to spaces for rendering purposes only
func expandTabsForDisplay(line []rune, tabWidth int) string {
	var result []rune
	for _, r := range line {
		if r == '\t' {
			// Add spaces to reach next tab stop
			spacesToAdd := tabWidth - (len(result) % tabWidth)
			for i := 0; i < spacesToAdd; i++ {
				result = append(result, ' ')
			}
		} else {
			result = append(result, r)
		}
	}
	return string(result)
}

func displayColumn(line []rune, x int) int {
	x = clamp(x, 0, len(line))
	col := 0
	for _, r := range line[:x] {
		if r == '\t' {
			col += tabWidth - (col % tabWidth)
		} else {
			col++
		}
	}
	return col
}

func cursorFromDisplayColumn(line []rune, targetCol int) int {
	if targetCol <= 0 {
		return 0
	}
	col := 0
	bestX := 0
	bestDiff := targetCol
	for i, r := range line {
		nextCol := col + 1
		if r == '\t' {
			nextCol = col + tabWidth - (col % tabWidth)
		}
		diff := nextCol - targetCol
		if diff < 0 {
			diff = -diff
		}
		if diff < bestDiff {
			bestDiff = diff
			bestX = i + 1
		}
		if nextCol >= targetCol {
			prevDiff := targetCol - col
			if prevDiff < 0 {
				prevDiff = -prevDiff
			}
			if prevDiff <= diff {
				return i
			}
			return i + 1
		}
		col = nextCol
	}
	return bestX
}

func (e *Editor) pixelWidthForRunes(line []rune) int {
	return displayColumn(line, len(line)) * e.charWidth
}

func (e *Editor) pixelWidthBefore(line []rune, x int) int {
	return displayColumn(line, x) * e.charWidth
}

func (e *Editor) positionOffset(pos Position) int {
	return e.buffer.PositionOffset(pos)
}

func (e *Editor) line(y int) []rune {
	return e.buffer.Line(y)
}

func (e *Editor) lineCount() int {
	return e.buffer.LineCount()
}

func (e *Editor) invalidateFileBrowserWidth() {
	e.fileBrowserMaxWidthDirty = true
}

func (e *Editor) getFileBrowserMaxWidth() int {
	if !e.fileBrowserMaxWidthDirty {
		return e.fileBrowserMaxWidth
	}
	maxWidth := 0
	for _, node := range e.flatFileList {
		indent := node.depth * 12
		iconWidth := 2 * e.charWidth
		nameLen := len([]rune(node.name))
		if !node.isDir && e.fileDirty(node.path) {
			nameLen += 2
		}
		totalWidth := 5 + indent + iconWidth + nameLen*e.charWidth
		if totalWidth > maxWidth {
			maxWidth = totalWidth
		}
	}
	e.fileBrowserMaxWidth = maxWidth
	e.fileBrowserMaxWidthDirty = false
	return maxWidth
}

func textCacheKey(text string, color sdl.Color) string {
	return fmt.Sprintf("%d:%d:%d:%d:%s", color.R, color.G, color.B, color.A, text)
}

func (e *Editor) cachedText(text string, color sdl.Color) (*CachedText, error) {
	key := textCacheKey(text, color)
	e.textCacheClock++
	if cached, ok := e.textCache[key]; ok {
		cached.lastUsed = e.textCacheClock
		return cached, nil
	}
	surface, err := e.font.RenderUTF8Blended(text, color)
	if err != nil {
		return nil, err
	}
	defer surface.Free()
	texture, err := e.renderer.CreateTextureFromSurface(surface)
	if err != nil {
		return nil, err
	}
	if len(e.textCache) >= textCacheLimit {
		var oldestKey string
		var oldest *CachedText
		for candidateKey, candidate := range e.textCache {
			if oldest == nil || candidate.lastUsed < oldest.lastUsed {
				oldestKey = candidateKey
				oldest = candidate
			}
		}
		if oldest != nil {
			oldest.texture.Destroy()
			delete(e.textCache, oldestKey)
		}
	}
	cached := &CachedText{texture: texture, w: surface.W, h: surface.H, lastUsed: e.textCacheClock}
	e.textCache[key] = cached
	return cached, nil
}

func (e *Editor) renderFileBrowser() {
	// Render file browser panel background
	e.renderer.SetDrawColor(20, 20, 20, 255)
	e.renderer.FillRect(&sdl.Rect{X: 0, Y: 0, W: int32(e.fileBrowserWidth), H: e.windowHeight})

	// Render file browser separator
	e.renderer.SetDrawColor(50, 50, 50, 255)
	e.renderer.DrawLine(int32(e.fileBrowserWidth), 0, int32(e.fileBrowserWidth), e.windowHeight)

	// Set clip rect for file browser to prevent overflow
	fileBrowserClipRect := &sdl.Rect{
		X: 0,
		Y: 0,
		W: int32(e.fileBrowserWidth),
		H: e.windowHeight,
	}
	e.renderer.SetClipRect(fileBrowserClipRect)

	// Get the highlighted node
	highlightedNode := e.getHighlightedNode()

	// Render file browser items
	filterOffset := 0
	if e.fileFilter != "" {
		filterText := "/" + e.fileFilter
		cached, err := e.cachedText(filterText, sdl.Color{R: 120, G: 180, B: 220, A: 255})
		if err == nil {
			e.renderer.Copy(cached.texture, nil, &sdl.Rect{X: 6, Y: 2, W: cached.w, H: cached.h})
		}
		filterOffset = lineHeight
	}
	fileBrowserVisibleLines := (int(e.windowHeight) - filterOffset) / lineHeight
	for i := e.fileBrowserScroll; i < len(e.flatFileList) && i < e.fileBrowserScroll+fileBrowserVisibleLines; i++ {
		node := e.flatFileList[i]
		y := filterOffset + (i-e.fileBrowserScroll)*lineHeight

		// Highlight if this is the active node
		if highlightedNode != nil && node.path == highlightedNode.path {
			e.renderer.SetDrawColor(40, 60, 80, 255)
			e.renderer.FillRect(&sdl.Rect{
				X: 0,
				Y: int32(y),
				W: int32(e.fileBrowserWidth),
				H: lineHeight,
			})
		}

		// Indent based on depth
		indent := node.depth * 12
		icon := ""
		if node.isDir {
			if node.expanded {
				icon = "▾ "
			} else {
				icon = "▸ "
			}
		} else {
			icon = "  "
		}

		// Check if this file has unsaved changes
		hasUnsavedChanges := false
		if !node.isDir {
			hasUnsavedChanges = e.fileDirty(node.path)
		}

		displayText := icon + node.name
		if hasUnsavedChanges {
			displayText += " *"
		}
		cached, err := e.cachedText(displayText, sdl.Color{R: 180, G: 180, B: 180, A: 255})
		if err != nil {
			continue
		}
		// Apply horizontal scroll to destination position
		dstX := int32(5 + indent - e.fileBrowserScrollX)
		rect := &sdl.Rect{X: dstX, Y: int32(y), W: cached.w, H: cached.h}
		e.renderer.Copy(cached.texture, nil, rect)
	}

	// Clear clip rect
	e.renderer.SetClipRect(nil)
}

func (e *Editor) insertRune(r rune) {
	insertOffset := e.positionOffset(Position{x: e.cursorX, y: e.cursorY})
	// Record undo operation
	op := UndoOp{
		deleteAt:     insertOffset,
		insertAt:     insertOffset,
		inserted:     []rune{r},
		cursorBefore: Position{x: e.cursorX, y: e.cursorY},
	}

	e.buffer.Insert(insertOffset, []rune{r})
	e.cursorX++

	e.idealCursorX = -1
	e.invalidateMaxLineWidth()
	e.recordUndo(op)
}

func (e *Editor) backspace() {
	if e.cursorX > 0 {
		deleteOffset := e.positionOffset(Position{x: e.cursorX - 1, y: e.cursorY})
		// Delete character before cursor
		line := e.line(e.cursorY)
		deletedChar := line[e.cursorX-1]

		// Record undo operation
		op := UndoOp{
			deleteAt:     deleteOffset,
			deleted:      []rune{deletedChar},
			insertAt:     deleteOffset,
			cursorBefore: Position{x: e.cursorX, y: e.cursorY},
		}

		e.buffer.Delete(deleteOffset, 1)
		e.cursorX--

		e.idealCursorX = -1
		e.invalidateMaxLineWidth()
		e.recordUndo(op)
	} else if e.cursorY > 0 {
		prevLine := e.line(e.cursorY - 1)
		deleteOffset := e.positionOffset(Position{x: len(prevLine), y: e.cursorY - 1})

		op := UndoOp{
			deleteAt:     deleteOffset,
			deleted:      []rune{'\n'},
			insertAt:     deleteOffset,
			cursorBefore: Position{x: e.cursorX, y: e.cursorY},
		}

		e.cursorX = len(prevLine)
		e.buffer.Delete(deleteOffset, 1)
		e.cursorY--

		e.idealCursorX = -1
		e.invalidateMaxLineWidth()
		e.recordUndo(op)
	}
}

func (e *Editor) deleteForward() {
	line := e.line(e.cursorY)
	if e.cursorX < len(line) {
		deleteOffset := e.positionOffset(Position{x: e.cursorX, y: e.cursorY})
		// Delete character at cursor
		deletedChar := line[e.cursorX]

		// Record undo operation
		op := UndoOp{
			deleteAt:     deleteOffset,
			deleted:      []rune{deletedChar},
			insertAt:     deleteOffset,
			cursorBefore: Position{x: e.cursorX, y: e.cursorY},
		}

		e.buffer.Delete(deleteOffset, 1)

		e.idealCursorX = -1
		e.invalidateMaxLineWidth()
		e.recordUndo(op)
	} else if e.cursorY < e.lineCount()-1 {
		deleteOffset := e.positionOffset(Position{x: len(line), y: e.cursorY})

		op := UndoOp{
			deleteAt:     deleteOffset,
			deleted:      []rune{'\n'},
			insertAt:     deleteOffset,
			cursorBefore: Position{x: e.cursorX, y: e.cursorY},
		}

		e.buffer.Delete(deleteOffset, 1)

		e.idealCursorX = -1
		e.invalidateMaxLineWidth()
		e.recordUndo(op)
	}
}

func (e *Editor) newline() {
	insertOffset := e.positionOffset(Position{x: e.cursorX, y: e.cursorY})

	op := UndoOp{
		deleteAt:     insertOffset,
		insertAt:     insertOffset,
		inserted:     []rune{'\n'},
		cursorBefore: Position{x: e.cursorX, y: e.cursorY},
	}

	e.buffer.Insert(insertOffset, []rune{'\n'})
	e.cursorY++
	e.cursorX = 0

	e.idealCursorX = -1
	e.invalidateMaxLineWidth()
	e.recordUndo(op)
}

func (e *Editor) updateDirtyFlag() {
	if e.savedAtUndoDepth == -1 {
		// Saved state is unreachable (we branched)
		e.isDirty = true
	} else {
		// Check if current undo stack depth matches saved state
		e.isDirty = (len(e.undoStack) != e.savedAtUndoDepth)
	}
	e.invalidateFileBrowserWidth()
}

func (e *Editor) recordUndo(op UndoOp) {
	// Set cursor position after operation
	op.cursorAfter = Position{x: e.cursorX, y: e.cursorY}
	op.deleted = append([]rune(nil), op.deleted...)
	op.inserted = append([]rune(nil), op.inserted...)

	// If we've undone past the save point and now make a new edit,
	// we're creating a new branch - saved state is unreachable
	if len(e.undoStack) < e.savedAtUndoDepth {
		e.savedAtUndoDepth = -1
	}

	e.undoStack = append(e.undoStack, op)
	// Clear redo stack when new action is performed
	e.redoStack = nil

	// Update dirty flag based on stack depth (but not during search)
	if !e.searchActive {
		e.updateDirtyFlag()
	}

	// Update search matches if search is active
	if e.searchActive {
		e.updateSearchMatches()
	}
	e.maybeAutosave()
}

func (e *Editor) undo() {
	if len(e.undoStack) == 0 {
		return
	}

	// Get the last operation
	op := e.undoStack[len(e.undoStack)-1]
	e.undoStack = e.undoStack[:len(e.undoStack)-1]

	// First: Delete what was inserted
	if len(op.inserted) > 0 {
		e.buffer.Delete(op.insertAt, len(op.inserted))
	}

	// Second: Insert back what was deleted
	if len(op.deleted) > 0 {
		e.buffer.Insert(op.deleteAt, op.deleted)
	}
	e.invalidateMaxLineWidth()

	// Restore cursor position to BEFORE the operation
	e.cursorX = op.cursorBefore.x
	e.cursorY = op.cursorBefore.y
	e.idealCursorX = -1

	// Push the ORIGINAL operation to redo stack so we can replay it
	e.redoStack = append(e.redoStack, op)

	// Update dirty flag based on new stack depth (but not during search)
	if !e.searchActive {
		e.updateDirtyFlag()
	} else {
		e.updateSearchMatches()
	}
}

func (e *Editor) redo() {
	if len(e.redoStack) == 0 {
		return
	}

	// Get the operation to redo (the original forward operation)
	op := e.redoStack[len(e.redoStack)-1]
	e.redoStack = e.redoStack[:len(e.redoStack)-1]

	// Apply it back to undo stack
	e.undoStack = append(e.undoStack, op)

	// Replay the original operation: delete first, then insert
	if len(op.deleted) > 0 {
		e.buffer.Delete(op.deleteAt, len(op.deleted))
	}
	if len(op.inserted) > 0 {
		e.buffer.Insert(op.insertAt, op.inserted)
	}
	e.invalidateMaxLineWidth()

	// Restore cursor position to AFTER the original operation
	e.cursorX = op.cursorAfter.x
	e.cursorY = op.cursorAfter.y
	e.idealCursorX = -1

	// Update dirty flag based on new stack depth (but not during search)
	if !e.searchActive {
		e.updateDirtyFlag()
	} else {
		e.updateSearchMatches()
	}
}

func (e *Editor) hasSelection() bool {
	if e.selectionStart == nil || e.selectionEnd == nil {
		return false
	}
	// Check if selection has non-zero length
	return e.selectionStart.x != e.selectionEnd.x || e.selectionStart.y != e.selectionEnd.y
}

func (e *Editor) clearSelection() {
	e.selectionStart = nil
	e.selectionEnd = nil
}

// getSelectionBounds returns start and end positions, ensuring start comes before end
func (e *Editor) getSelectionBounds() (start, end Position) {
	s, end := *e.selectionStart, *e.selectionEnd
	if s.y > end.y || (s.y == end.y && s.x > end.x) {
		return end, s
	}
	return s, end
}

// Find the character position closest to a target pixel X position
func (e *Editor) findCursorXFromPixel(lineY int, targetPixelX int) int {
	if lineY >= e.lineCount() {
		return 0
	}
	line := e.line(lineY)
	if targetPixelX < 0 {
		targetPixelX = 0
	}
	return cursorFromDisplayColumn(line, targetPixelX/e.charWidth)
}

func (e *Editor) getSelectedText() string {
	if !e.hasSelection() {
		return ""
	}

	start, end := e.getSelectionBounds()
	return string(e.buffer.TextRange(start, end))
}

func (e *Editor) deleteSelection() {
	if !e.hasSelection() {
		return
	}

	start, end := e.getSelectionBounds()

	deleteOffset := e.positionOffset(start)
	deleted := e.buffer.TextRange(start, end)

	// Record undo operation
	op := UndoOp{
		deleteAt:     deleteOffset,
		deleted:      deleted,
		insertAt:     deleteOffset,
		cursorBefore: Position{x: e.cursorX, y: e.cursorY},
	}

	e.buffer.Delete(deleteOffset, len(deleted))

	e.cursorX = start.x
	e.cursorY = start.y
	e.clearSelection()
	e.invalidateMaxLineWidth()

	e.recordUndo(op)
}

func (e *Editor) selectAll() {
	e.selectionStart = &Position{0, 0}
	lastLine := e.lineCount() - 1
	e.selectionEnd = &Position{len(e.line(lastLine)), lastLine}
}

func (e *Editor) moveToSelectionStart() {
	if e.hasSelection() {
		start, end := e.selectionStart, e.selectionEnd
		if start.y > end.y || (start.y == end.y && start.x > end.x) {
			start, end = end, start
		}
		e.cursorX = start.x
		e.cursorY = start.y
		e.clearSelection()
	}
}

func (e *Editor) moveToSelectionEnd() {
	if e.hasSelection() {
		start, end := e.selectionStart, e.selectionEnd
		if start.y > end.y || (start.y == end.y && start.x > end.x) {
			start, end = end, start
		}
		e.cursorX = end.x
		e.cursorY = end.y
		e.clearSelection()
	}
}

func (e *Editor) copy() {
	if e.hasSelection() {
		sdl.SetClipboardText(e.getSelectedText())
	}
}

func isWordChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
}

// isWordCharForSelection checks if a rune is part of a word for selection purposes
func isWordCharForSelection(r rune) bool {
	// Word characters are alphanumeric and underscore
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
}

// selectWord selects the word at the current cursor position
func (e *Editor) selectWord() {
	if e.cursorY >= e.lineCount() {
		return
	}

	line := e.line(e.cursorY)
	if len(line) == 0 {
		return
	}

	// If cursor is beyond line end or on a non-word character, select nothing
	if e.cursorX >= len(line) || !isWordCharForSelection(line[e.cursorX]) {
		// Check if cursor is right after a word
		if e.cursorX > 0 && e.cursorX <= len(line) && isWordCharForSelection(line[e.cursorX-1]) {
			e.cursorX--
		} else {
			return
		}
	}

	// Find start of word
	start := e.cursorX
	for start > 0 && isWordCharForSelection(line[start-1]) {
		start--
	}

	// Find end of word
	end := e.cursorX
	for end < len(line) && isWordCharForSelection(line[end]) {
		end++
	}

	// Set selection
	e.selectionStart = &Position{x: start, y: e.cursorY}
	e.selectionEnd = &Position{x: end, y: e.cursorY}
	e.cursorX = end
}

// selectLine selects the entire current line
func (e *Editor) selectLine() {
	if e.cursorY >= e.lineCount() {
		return
	}

	// Select from start to end of line
	e.selectionStart = &Position{x: 0, y: e.cursorY}
	e.selectionEnd = &Position{x: len(e.line(e.cursorY)), y: e.cursorY}
	e.cursorX = len(e.line(e.cursorY))
}

func (e *Editor) moveWordLeft() {
	if e.cursorX == 0 {
		if e.cursorY > 0 {
			e.cursorY--
			e.cursorX = len(e.line(e.cursorY))
		}
		return
	}

	line := e.line(e.cursorY)
	e.cursorX--

	// Skip whitespace
	for e.cursorX > 0 && !isWordChar(line[e.cursorX]) && line[e.cursorX] != ' ' {
		e.cursorX--
	}
	for e.cursorX > 0 && line[e.cursorX] == ' ' {
		e.cursorX--
	}

	// Skip word characters
	for e.cursorX > 0 && isWordChar(line[e.cursorX-1]) {
		e.cursorX--
	}
}

func (e *Editor) moveWordRight() {
	line := e.line(e.cursorY)
	if e.cursorX >= len(line) {
		if e.cursorY < e.lineCount()-1 {
			e.cursorY++
			e.cursorX = 0
		}
		return
	}

	// Skip current word
	for e.cursorX < len(line) && isWordChar(line[e.cursorX]) {
		e.cursorX++
	}

	// Skip whitespace and punctuation
	for e.cursorX < len(line) && !isWordChar(line[e.cursorX]) {
		e.cursorX++
	}
}

func (e *Editor) getCursorFromMouse(x, y int32) (int, int) {
	// Calculate line from y position (accounting for scroll)
	top := tabBarHeight
	if e.searchActive {
		top += 30
	}
	lineY := (int(y)-top)/lineHeight + e.scrollOffsetY
	if lineY < 0 {
		lineY = 0
	}
	if lineY >= e.lineCount() {
		lineY = e.lineCount() - 1
	}

	// Adjust x for file browser and gutter, and add horizontal scroll
	adjustedX := int(x) - e.fileBrowserWidth - gutterWidth + e.scrollOffsetX

	if adjustedX < 0 {
		return 0, lineY
	}

	line := e.line(lineY)
	return cursorFromDisplayColumn(line, adjustedX/e.charWidth), lineY
}

func (e *Editor) getVisibleLines() int {
	used := tabBarHeight
	if e.searchActive {
		used += 30
	}
	return max(1, (int(e.windowHeight)-used)/lineHeight)
}

func (e *Editor) getCursorPixelX() int {
	if e.cursorX == 0 || e.cursorY >= e.lineCount() {
		return 0
	}
	return e.pixelWidthBefore(e.line(e.cursorY), e.cursorX)
}

// getMaxLineWidth returns the cached maximum line width, recalculating if needed
func (e *Editor) getMaxLineWidth() int {
	if !e.maxLineWidthDirty {
		return e.maxLineWidth
	}

	// Recalculate max line width
	maxWidth := 0
	for y := 0; y < e.lineCount(); y++ {
		line := e.line(y)
		w := e.pixelWidthForRunes(line)
		if w > maxWidth {
			maxWidth = w
		}
	}

	e.maxLineWidth = maxWidth
	e.maxLineWidthDirty = false
	return maxWidth
}

// invalidateMaxLineWidth marks the cached max line width as needing recalculation
func (e *Editor) invalidateMaxLineWidth() {
	e.maxLineWidthDirty = true
}

func (e *Editor) ensureCursorVisible() {
	visibleLines := e.getVisibleLines()

	// Vertical scrolling
	// Scroll down if cursor is below visible area
	if e.cursorY >= e.scrollOffsetY+visibleLines {
		e.scrollOffsetY = e.cursorY - visibleLines + 1
	}

	// Scroll up if cursor is above visible area
	if e.cursorY < e.scrollOffsetY {
		e.scrollOffsetY = e.cursorY
	}

	// Clamp vertical scroll offset
	maxScroll := max(0, e.lineCount()-visibleLines)
	e.scrollOffsetY = clamp(e.scrollOffsetY, 0, maxScroll)

	// Horizontal scrolling
	cursorPixelX := e.getCursorPixelX()
	codeAreaWidth := int(e.windowWidth) - e.fileBrowserWidth - gutterWidth - 20

	// Scroll right if cursor is beyond visible area
	if cursorPixelX >= e.scrollOffsetX+codeAreaWidth {
		e.scrollOffsetX = cursorPixelX - codeAreaWidth + 50
	}

	// Scroll left if cursor is before visible area
	if cursorPixelX < e.scrollOffsetX {
		e.scrollOffsetX = cursorPixelX - 50
	}

	e.scrollOffsetX = max(0, e.scrollOffsetX)
}

// centerCursorOnScreen scrolls to center the cursor vertically and horizontally on screen
func (e *Editor) centerCursorOnScreen() {
	visibleLines := e.getVisibleLines()

	// Center vertically
	e.scrollOffsetY = e.cursorY - visibleLines/2

	// Clamp vertical scroll offset
	maxScroll := max(0, e.lineCount()-visibleLines)
	e.scrollOffsetY = clamp(e.scrollOffsetY, 0, maxScroll)

	// Center horizontally
	cursorPixelX := e.getCursorPixelX()
	codeAreaWidth := int(e.windowWidth) - e.fileBrowserWidth - gutterWidth - 20

	e.scrollOffsetX = cursorPixelX - codeAreaWidth/2

	e.scrollOffsetX = max(0, e.scrollOffsetX)
}

func (e *Editor) scroll(delta int) {
	e.scrollOffsetY += delta
	visibleLines := e.getVisibleLines()
	maxScroll := max(0, e.lineCount()-visibleLines)
	e.scrollOffsetY = clamp(e.scrollOffsetY, 0, maxScroll)
}

func (e *Editor) currentKey() string {
	if e.untitled {
		return e.untitledName
	}
	return e.currentFile
}

func (e *Editor) displayName(path string) string {
	if path == "" {
		if e.untitled {
			return e.untitledName
		}
		return ""
	}
	return filepath.Base(path)
}

func (e *Editor) rememberCurrentState() {
	key := e.currentKey()
	if key == "" {
		return
	}
	e.fileStates[key] = &FileState{
		buffer:           e.buffer.Clone(),
		cursor:           Position{x: e.cursorX, y: e.cursorY},
		scroll:           Position{x: e.scrollOffsetX, y: e.scrollOffsetY},
		undoStack:        cloneUndoStack(e.undoStack),
		redoStack:        cloneUndoStack(e.redoStack),
		savedAtUndoDepth: e.savedAtUndoDepth,
		dirty:            e.isDirty,
		untitled:         e.untitled,
	}
}

func (e *Editor) addOpenFile(path string) {
	if path == "" {
		return
	}
	for _, open := range e.openFiles {
		if open == path {
			return
		}
	}
	e.openFiles = append(e.openFiles, path)
}

func (e *Editor) removeOpenFile(path string) {
	for i, open := range e.openFiles {
		if open == path {
			e.openFiles = append(e.openFiles[:i], e.openFiles[i+1:]...)
			return
		}
	}
}

func (e *Editor) autosavePath(path string) string {
	clean := filepath.Clean(path)
	name := strings.Map(func(r rune) rune {
		if r == ':' || r == '\\' || r == '/' || r == '?' || r == '*' || r == '"' || r == '<' || r == '>' || r == '|' {
			return '_'
		}
		return r
	}, clean)
	return filepath.Join(e.stateDir, "autosave", name+".autosave")
}

func (e *Editor) maybeAutosave() {
	if e.currentFile == "" || !e.isDirty || time.Since(e.lastAutosave) < e.autosaveInterval {
		return
	}
	_ = os.WriteFile(e.autosavePath(e.currentFile), []byte(e.buffer.String()), 0644)
	e.lastAutosave = time.Now()
}

func (e *Editor) clearAutosave(path string) {
	if path != "" {
		_ = os.Remove(e.autosavePath(path))
	}
}

func (e *Editor) saveSession() {
	e.rememberCurrentState()
	session := SessionState{
		Root:        e.rootDir,
		OpenFiles:   append([]string(nil), e.openFiles...),
		CurrentFile: e.currentFile,
	}
	data, err := json.MarshalIndent(session, "", "  ")
	if err == nil {
		_ = os.WriteFile(filepath.Join(e.stateDir, "session.json"), data, 0644)
	}
}

func (e *Editor) restoreSession() {
	data, err := os.ReadFile(filepath.Join(e.stateDir, "session.json"))
	if err != nil {
		return
	}
	var session SessionState
	if json.Unmarshal(data, &session) != nil || session.Root != e.rootDir {
		return
	}
	for _, path := range session.OpenFiles {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			e.addOpenFile(path)
		}
	}
	if session.CurrentFile != "" {
		_ = e.loadFile(session.CurrentFile)
	}
}

func (e *Editor) saveFile() error {
	if e.untitled {
		e.beginOverlay(overlaySaveAs, "Save as", "")
		return nil
	}
	if e.currentFile == "" {
		return fmt.Errorf("no file is currently open")
	}

	// Write to file
	err := os.WriteFile(e.currentFile, []byte(e.buffer.String()), 0644)
	if err != nil {
		return err
	}

	// Record the undo stack depth at save time
	e.savedAtUndoDepth = len(e.undoStack)
	e.isDirty = false
	if state, ok := e.fileStates[e.currentFile]; ok {
		state.dirty = false
		state.savedAtUndoDepth = e.savedAtUndoDepth
	}
	e.clearAutosave(e.currentFile)
	e.invalidateFileBrowserWidth()
	e.saveSession()

	return nil
}

func (e *Editor) loadFile(path string) error {
	path, _ = filepath.Abs(path)
	e.rememberCurrentState()

	if savedState, exists := e.fileStates[path]; exists {
		e.buffer = savedState.buffer.Clone()
		e.cursorX = savedState.cursor.x
		e.cursorY = savedState.cursor.y
		e.scrollOffsetX = savedState.scroll.x
		e.scrollOffsetY = savedState.scroll.y
		e.currentFile = path
		e.untitled = false
		e.untitledName = ""
		e.isDirty = savedState.dirty
		e.savedAtUndoDepth = savedState.savedAtUndoDepth
		e.clearSelection()
		e.undoStack = cloneUndoStack(savedState.undoStack)
		e.redoStack = cloneUndoStack(savedState.redoStack)
		e.invalidateMaxLineWidth()
		e.invalidateFileBrowserWidth()
		e.addOpenFile(path)
		e.saveSession()
		return nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	loadedDirty := false
	if autoInfo, err := os.Stat(e.autosavePath(path)); err == nil {
		if diskInfo, diskErr := os.Stat(path); diskErr != nil || autoInfo.ModTime().After(diskInfo.ModTime()) {
			if autosaved, readErr := os.ReadFile(e.autosavePath(path)); readErr == nil {
				content = autosaved
				loadedDirty = true
			}
		}
	}

	// Clear undo/redo stacks
	e.undoStack = nil
	e.redoStack = nil

	// Convert content to string to properly handle UTF-8 multibyte characters
	contentStr := string(content)
	e.buffer = NewPieceTable(contentStr)

	e.currentFile = path
	e.untitled = false
	e.untitledName = ""
	e.cursorX = 0
	e.cursorY = 0
	e.scrollOffsetX = 0
	e.scrollOffsetY = 0
	e.isDirty = loadedDirty
	e.savedAtUndoDepth = 0 // Empty undo stack = just loaded state
	e.clearSelection()
	e.invalidateMaxLineWidth()
	e.invalidateFileBrowserWidth()
	e.addOpenFile(path)
	e.saveSession()

	return nil
}

func (e *Editor) handleFileBrowserClick(x, y int32) {
	if x >= int32(e.fileBrowserWidth) {
		return
	}

	// Calculate which file was clicked
	offset := 0
	if e.fileFilter != "" {
		offset = lineHeight
	}
	if int(y) < offset {
		return
	}
	clickedLine := (int(y) - offset) / lineHeight
	clickedIndex := e.fileBrowserScroll + clickedLine

	if clickedIndex >= 0 && clickedIndex < len(e.flatFileList) {
		node := e.flatFileList[clickedIndex]

		if node.isDir {
			// Toggle directory expansion
			node.expanded = !node.expanded
			if node.expanded && len(node.children) == 0 {
				e.readDirectory(node)
			}
			e.flattenTree()
		} else {
			// Load file
			e.loadFile(node.path)
		}
	}
}

func (e *Editor) resolvePath(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	if filepath.IsAbs(input) {
		return filepath.Clean(input)
	}
	return filepath.Join(e.rootDir, input)
}

func (e *Editor) beginOverlay(mode OverlayMode, title, text string) {
	e.overlayActive = true
	e.overlayMode = mode
	e.overlayTitle = title
	e.overlayText = text
	e.clearSelection()
}

func (e *Editor) closeOverlay() {
	e.overlayActive = false
	e.overlayMode = overlayNone
	e.overlayTitle = ""
	e.overlayText = ""
}

func (e *Editor) newUntitled() {
	e.rememberCurrentState()
	e.untitledName = fmt.Sprintf("untitled-%d", e.untitledCounter)
	e.untitledCounter++
	e.untitled = true
	e.currentFile = ""
	e.buffer = NewPieceTable("")
	e.cursorX, e.cursorY = 0, 0
	e.scrollOffsetX, e.scrollOffsetY = 0, 0
	e.undoStack, e.redoStack = nil, nil
	e.isDirty = true
	e.savedAtUndoDepth = -1
	e.clearSelection()
	e.invalidateMaxLineWidth()
}

func (e *Editor) saveAs(path string) error {
	path = e.resolvePath(path)
	if path == "" {
		return fmt.Errorf("empty path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(e.buffer.String()), 0644); err != nil {
		return err
	}
	oldKey := e.currentKey()
	delete(e.fileStates, oldKey)
	e.currentFile = path
	e.untitled = false
	e.untitledName = ""
	e.savedAtUndoDepth = len(e.undoStack)
	e.isDirty = false
	e.addOpenFile(path)
	e.clearAutosave(path)
	e.buildFileTree()
	e.saveSession()
	return nil
}

func (e *Editor) createFile(path string) error {
	path = e.resolvePath(path)
	if path == "" {
		return fmt.Errorf("empty path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	_ = file.Close()
	e.buildFileTree()
	return e.loadFile(path)
}

func (e *Editor) renameCurrent(path string) error {
	if e.currentFile == "" {
		return fmt.Errorf("no file is currently open")
	}
	path = e.resolvePath(path)
	if path == "" {
		return fmt.Errorf("empty path")
	}
	e.rememberCurrentState()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := os.Rename(e.currentFile, path); err != nil {
		return err
	}
	state := e.fileStates[e.currentFile]
	delete(e.fileStates, e.currentFile)
	if state != nil {
		e.fileStates[path] = state
	}
	e.removeOpenFile(e.currentFile)
	e.currentFile = path
	e.addOpenFile(path)
	e.buildFileTree()
	e.saveSession()
	return nil
}

func (e *Editor) deleteCurrentFile() error {
	if e.currentFile == "" {
		return fmt.Errorf("no file is currently open")
	}
	path := e.currentFile
	if err := os.Remove(path); err != nil {
		return err
	}
	delete(e.fileStates, path)
	e.removeOpenFile(path)
	e.clearAutosave(path)
	e.currentFile = ""
	e.untitled = false
	e.buffer = NewPieceTable("")
	e.isDirty = false
	e.buildFileTree()
	if len(e.openFiles) > 0 {
		return e.loadFile(e.openFiles[len(e.openFiles)-1])
	}
	e.saveSession()
	return nil
}

func (e *Editor) executeOverlay() {
	text := strings.TrimSpace(e.overlayText)
	mode := e.overlayMode
	e.closeOverlay()
	var err error
	switch mode {
	case overlayCommand:
		err = e.executeCommand(text)
	case overlayOpen:
		err = e.loadFile(e.resolvePath(text))
	case overlayCreate:
		err = e.createFile(text)
	case overlayRename:
		err = e.renameCurrent(text)
	case overlayDelete:
		if strings.EqualFold(text, "yes") || strings.EqualFold(text, "y") {
			err = e.deleteCurrentFile()
		}
	case overlayGoto:
		var line int
		line, err = strconv.Atoi(text)
		if err == nil {
			e.cursorY = clamp(line-1, 0, e.lineCount()-1)
			e.cursorX = clamp(e.cursorX, 0, len(e.line(e.cursorY)))
			e.centerCursorOnScreen()
		}
	case overlayFilter:
		e.fileFilter = text
		e.flattenTree()
	case overlaySaveAs:
		err = e.saveAs(text)
	case overlayReplace:
		e.searchReplace = text
		e.replaceCurrentMatch()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", programName, err)
	}
}

func (e *Editor) executeCommand(command string) error {
	command = strings.ToLower(strings.TrimSpace(command))
	switch {
	case command == "new" || command == "new file":
		e.newUntitled()
	case command == "open":
		e.beginOverlay(overlayOpen, "Open file", "")
	case command == "create":
		e.beginOverlay(overlayCreate, "Create file", "")
	case command == "rename":
		e.beginOverlay(overlayRename, "Rename current file", e.currentFile)
	case command == "delete":
		e.beginOverlay(overlayDelete, "Delete current file? type yes", "")
	case command == "goto" || command == "go to line":
		e.beginOverlay(overlayGoto, "Go to line", "")
	case command == "find":
		e.activateSearch()
	case command == "replace":
		e.beginOverlay(overlayReplace, "Replace current match with", e.searchReplace)
	case command == "replace all":
		e.replaceAllMatches()
	case command == "filter":
		e.beginOverlay(overlayFilter, "Filter files", e.fileFilter)
	case command == "refresh":
		e.buildFileTree()
	case command == "save":
		return e.saveFile()
	case command == "save as":
		e.beginOverlay(overlaySaveAs, "Save as", e.currentFile)
	case command == "next tab":
		e.switchTab(1)
	case command == "previous tab":
		e.switchTab(-1)
	case command == "close tab":
		e.closeCurrentTab()
	case command == "duplicate line":
		e.duplicateLine()
	case command == "delete line":
		e.deleteLine()
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	return nil
}

func (e *Editor) paste() {
	clipboardText, _ := sdl.GetClipboardText()
	if clipboardText == "" {
		return
	}

	if e.hasSelection() {
		e.deleteSelection()
	}
	insertOffset := e.positionOffset(Position{x: e.cursorX, y: e.cursorY})

	// Record as single undo operation
	pastedText := []rune(clipboardText)
	op := UndoOp{
		deleteAt:     insertOffset,
		insertAt:     insertOffset,
		inserted:     pastedText,
		cursorBefore: Position{x: e.cursorX, y: e.cursorY},
	}

	// Apply the paste
	e.buffer.Insert(insertOffset, pastedText)
	newlineCount := 0
	lastLineLength := 0
	for _, r := range pastedText {
		if r == '\n' {
			newlineCount++
			lastLineLength = 0
		} else {
			lastLineLength++
		}
	}
	if newlineCount == 0 {
		e.cursorX += lastLineLength
	} else {
		e.cursorY += newlineCount
		e.cursorX = lastLineLength
	}

	e.idealCursorX = -1
	e.invalidateMaxLineWidth()
	e.recordUndo(op)
}

func (e *Editor) switchTab(delta int) {
	if len(e.openFiles) == 0 {
		return
	}
	current := e.currentFile
	idx := 0
	for i, path := range e.openFiles {
		if path == current {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(e.openFiles)) % len(e.openFiles)
	_ = e.loadFile(e.openFiles[idx])
}

func (e *Editor) closeCurrentTab() {
	if e.currentFile == "" {
		e.untitled = false
		e.untitledName = ""
		e.buffer = NewPieceTable("")
		e.isDirty = false
		return
	}
	closing := e.currentFile
	delete(e.fileStates, closing)
	e.removeOpenFile(closing)
	e.currentFile = ""
	e.buffer = NewPieceTable("")
	e.isDirty = false
	if len(e.openFiles) > 0 {
		_ = e.loadFile(e.openFiles[len(e.openFiles)-1])
	}
	e.saveSession()
}

func (e *Editor) duplicateLine() {
	line := append([]rune(nil), e.line(e.cursorY)...)
	insert := append([]rune{'\n'}, line...)
	offset := e.positionOffset(Position{x: len(e.line(e.cursorY)), y: e.cursorY})
	op := UndoOp{
		deleteAt:     offset,
		insertAt:     offset,
		inserted:     insert,
		cursorBefore: Position{x: e.cursorX, y: e.cursorY},
	}
	e.buffer.Insert(offset, insert)
	e.cursorY++
	e.recordUndo(op)
	e.invalidateMaxLineWidth()
}

func (e *Editor) deleteLine() {
	start := e.positionOffset(Position{x: 0, y: e.cursorY})
	end := e.positionOffset(Position{x: len(e.line(e.cursorY)), y: e.cursorY})
	if e.cursorY < e.lineCount()-1 {
		end++
	} else if e.cursorY > 0 {
		start--
	}
	deleted := e.buffer.Slice(start, end-start)
	if len(deleted) == 0 {
		return
	}
	op := UndoOp{
		deleteAt:     start,
		deleted:      deleted,
		insertAt:     start,
		cursorBefore: Position{x: e.cursorX, y: e.cursorY},
	}
	e.buffer.Delete(start, len(deleted))
	e.cursorY = clamp(e.cursorY, 0, e.lineCount()-1)
	e.cursorX = clamp(e.cursorX, 0, len(e.line(e.cursorY)))
	e.recordUndo(op)
	e.invalidateMaxLineWidth()
}

func (e *Editor) moveLine(delta int) {
	if e.lineCount() < 2 {
		return
	}
	target := e.cursorY + delta
	if target < 0 || target >= e.lineCount() {
		return
	}
	lines := strings.Split(e.buffer.String(), "\n")
	lines[e.cursorY], lines[target] = lines[target], lines[e.cursorY]
	old := []rune(e.buffer.String())
	newText := []rune(strings.Join(lines, "\n"))
	op := UndoOp{
		deleteAt:     0,
		deleted:      old,
		insertAt:     0,
		inserted:     newText,
		cursorBefore: Position{x: e.cursorX, y: e.cursorY},
	}
	e.buffer.Delete(0, e.buffer.Len())
	e.buffer.Insert(0, newText)
	e.cursorY = target
	e.recordUndo(op)
	e.invalidateMaxLineWidth()
}

func (e *Editor) indentSelection(prefix string) {
	startLine, endLine := e.cursorY, e.cursorY
	if e.hasSelection() {
		start, end := e.getSelectionBounds()
		startLine, endLine = start.y, end.y
	}
	old := []rune(e.buffer.String())
	lines := strings.Split(e.buffer.String(), "\n")
	for y := startLine; y <= endLine; y++ {
		lines[y] = prefix + lines[y]
	}
	newText := []rune(strings.Join(lines, "\n"))
	op := UndoOp{
		deleteAt:     0,
		deleted:      old,
		insertAt:     0,
		inserted:     newText,
		cursorBefore: Position{x: e.cursorX, y: e.cursorY},
	}
	e.buffer.Delete(0, e.buffer.Len())
	e.buffer.Insert(0, newText)
	e.cursorX += len(prefix)
	e.recordUndo(op)
	e.invalidateMaxLineWidth()
}

func (e *Editor) unindentSelection(prefix string) {
	startLine, endLine := e.cursorY, e.cursorY
	if e.hasSelection() {
		start, end := e.getSelectionBounds()
		startLine, endLine = start.y, end.y
	}
	old := []rune(e.buffer.String())
	lines := strings.Split(e.buffer.String(), "\n")
	changed := false
	for y := startLine; y <= endLine; y++ {
		if strings.HasPrefix(lines[y], prefix) {
			lines[y] = strings.TrimPrefix(lines[y], prefix)
			changed = true
		}
	}
	if !changed {
		return
	}
	newText := []rune(strings.Join(lines, "\n"))
	op := UndoOp{deleteAt: 0, deleted: old, insertAt: 0, inserted: newText, cursorBefore: Position{x: e.cursorX, y: e.cursorY}}
	e.buffer.Delete(0, e.buffer.Len())
	e.buffer.Insert(0, newText)
	e.cursorX = max(0, e.cursorX-len(prefix))
	e.recordUndo(op)
	e.invalidateMaxLineWidth()
}

func (e *Editor) replaceCurrentMatch() {
	if !e.searchActive || e.savedEditorState == nil || len(e.searchMatches) == 0 || e.currentMatchIndex < 0 {
		return
	}
	match := e.searchMatches[e.currentMatchIndex]
	queryLen := len(e.buffer.Line(0))
	if e.currentMatchIndex >= 0 && e.currentMatchIndex < len(e.searchMatchLens) {
		queryLen = e.searchMatchLens[e.currentMatchIndex]
	}
	e.deactivateSearch()
	start := e.positionOffset(match)
	deleted := e.buffer.Slice(start, queryLen)
	inserted := []rune(e.searchReplace)
	op := UndoOp{deleteAt: start, deleted: deleted, insertAt: start, inserted: inserted, cursorBefore: Position{x: e.cursorX, y: e.cursorY}}
	e.buffer.Delete(start, len(deleted))
	e.buffer.Insert(start, inserted)
	e.cursorX = match.x + len(inserted)
	e.cursorY = match.y
	e.recordUndo(op)
	e.invalidateMaxLineWidth()
}

func (e *Editor) replaceAllMatches() {
	if !e.searchActive || e.savedEditorState == nil || len(e.searchMatches) == 0 {
		return
	}
	queryLen := len(e.buffer.Line(0))
	lens := append([]int(nil), e.searchMatchLens...)
	matches := append([]Position(nil), e.searchMatches...)
	e.deactivateSearch()
	old := []rune(e.buffer.String())
	for i := len(matches) - 1; i >= 0; i-- {
		offset := e.positionOffset(matches[i])
		length := queryLen
		if i < len(lens) {
			length = lens[i]
		}
		e.buffer.Delete(offset, length)
		e.buffer.Insert(offset, []rune(e.searchReplace))
	}
	newText := []rune(e.buffer.String())
	op := UndoOp{deleteAt: 0, deleted: old, insertAt: 0, inserted: newText, cursorBefore: Position{x: e.cursorX, y: e.cursorY}}
	e.recordUndo(op)
	e.invalidateMaxLineWidth()
}

// activateSearch saves current editor state and sets up search mode
func (e *Editor) activateSearch() {
	if e.currentFile == "" && !e.untitled {
		return
	}
	// Save current editor state including undo/redo stacks
	e.savedEditorState = &FileState{
		buffer:           e.buffer.Clone(),
		cursor:           Position{x: e.cursorX, y: e.cursorY},
		scroll:           Position{x: e.scrollOffsetX, y: e.scrollOffsetY},
		undoStack:        cloneUndoStack(e.undoStack),
		redoStack:        cloneUndoStack(e.redoStack),
		savedAtUndoDepth: e.savedAtUndoDepth,
	}

	// Set up search mode with single empty line and fresh undo/redo stacks
	e.searchActive = true
	e.buffer = NewPieceTable("")
	e.cursorX = 0
	e.cursorY = 0
	e.scrollOffsetX = 0
	e.scrollOffsetY = 0
	e.searchMatches = nil
	e.searchMatchLens = nil
	e.currentMatchIndex = -1
	e.clearSelection()
	e.undoStack = nil
	e.redoStack = nil
	e.savedAtUndoDepth = 0
}

// deactivateSearch restores editor state and closes search
func (e *Editor) deactivateSearch() {
	if e.savedEditorState == nil {
		return
	}

	// Restore editor state including undo/redo stacks
	e.buffer = e.savedEditorState.buffer.Clone()
	e.cursorX = e.savedEditorState.cursor.x
	e.cursorY = e.savedEditorState.cursor.y
	e.scrollOffsetX = e.savedEditorState.scroll.x
	e.scrollOffsetY = e.savedEditorState.scroll.y
	e.undoStack = cloneUndoStack(e.savedEditorState.undoStack)
	e.redoStack = cloneUndoStack(e.savedEditorState.redoStack)
	e.savedAtUndoDepth = e.savedEditorState.savedAtUndoDepth
	e.updateDirtyFlag()

	// Clear search state
	e.searchActive = false
	e.searchMatches = nil
	e.searchMatchLens = nil
	e.currentMatchIndex = -1
	e.savedEditorState = nil
	e.clearSelection()
	e.invalidateMaxLineWidth()
}

// updateSearchMatches finds all matches in saved editor state based on current search query
func (e *Editor) updateSearchMatches() {
	if !e.searchActive || e.savedEditorState == nil {
		return
	}

	e.searchMatches = nil
	e.searchMatchLens = nil
	e.currentMatchIndex = -1

	searchQuery := e.buffer.Line(0)
	if len(searchQuery) == 0 {
		return
	}

	queryLen := len(searchQuery)
	queryText := string(searchQuery)
	var rx *regexp.Regexp
	if e.searchRegex {
		pattern := queryText
		if !e.searchCaseSensitive {
			pattern = "(?i)" + pattern
		}
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return
		}
		rx = compiled
	}

	// Search through saved editor lines
	for y := 0; y < e.savedEditorState.buffer.LineCount(); y++ {
		line := e.savedEditorState.buffer.Line(y)
		if len(line) < queryLen {
			continue
		}
		if rx != nil {
			lineText := string(line)
			for _, match := range rx.FindAllStringIndex(lineText, -1) {
				start := len([]rune(lineText[:match[0]]))
				length := len([]rune(lineText[match[0]:match[1]]))
				if length > 0 && (!e.searchWholeWord || isWholeWordMatch(line, start, length)) {
					e.searchMatches = append(e.searchMatches, Position{x: start, y: y})
					e.searchMatchLens = append(e.searchMatchLens, length)
				}
			}
			continue
		}
		haystack := line
		needle := searchQuery
		if !e.searchCaseSensitive {
			haystack = lowerRunes(line)
			needle = lowerRunes(searchQuery)
		}
		for x := 0; x <= len(line)-queryLen; x++ {
			if runeSliceEqual(haystack[x:x+queryLen], needle) && (!e.searchWholeWord || isWholeWordMatch(line, x, queryLen)) {
				e.searchMatches = append(e.searchMatches, Position{x: x, y: y})
				e.searchMatchLens = append(e.searchMatchLens, queryLen)
			}
		}
	}

	// Set current match to first result if any found
	if len(e.searchMatches) > 0 {
		e.currentMatchIndex = 0
	}
}

func isWholeWordMatch(line []rune, start, length int) bool {
	before := start == 0 || !isWordChar(line[start-1])
	afterIndex := start + length
	after := afterIndex >= len(line) || !isWordChar(line[afterIndex])
	return before && after
}

func runeSliceEqual(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func lowerRunes(in []rune) []rune {
	out := make([]rune, len(in))
	for i, r := range in {
		out[i] = unicode.ToLower(r)
	}
	return out
}

func (e *Editor) visibleSearchMatchIndexes(startLine, endLine int) []int {
	if len(e.searchMatches) == 0 || startLine >= endLine {
		return nil
	}
	first := sort.Search(len(e.searchMatches), func(i int) bool {
		return e.searchMatches[i].y >= startLine
	})
	last := sort.Search(len(e.searchMatches), func(i int) bool {
		return e.searchMatches[i].y >= endLine
	})
	indexes := make([]int, 0, last-first)
	for i := first; i < last; i++ {
		indexes = append(indexes, i)
	}
	return indexes
}

// jumpToNextMatch moves cursor to the next search match
func (e *Editor) jumpToNextMatch() {
	if len(e.searchMatches) == 0 || !e.searchActive || e.savedEditorState == nil {
		return
	}

	// Cycle to next match
	e.currentMatchIndex = (e.currentMatchIndex + 1) % len(e.searchMatches)

	// Update saved editor state's cursor to the match
	match := e.searchMatches[e.currentMatchIndex]
	e.savedEditorState.cursor.x = match.x
	e.savedEditorState.cursor.y = match.y

	// Center the match in the view
	visibleLines := e.getVisibleLines()
	e.savedEditorState.scroll.y = match.y - visibleLines/2
	maxScroll := max(0, e.savedEditorState.buffer.LineCount()-visibleLines)
	e.savedEditorState.scroll.y = clamp(e.savedEditorState.scroll.y, 0, maxScroll)
}

// jumpToPreviousMatch moves cursor to the previous search match
func (e *Editor) jumpToPreviousMatch() {
	if len(e.searchMatches) == 0 || !e.searchActive || e.savedEditorState == nil {
		return
	}

	// Cycle to previous match
	e.currentMatchIndex--
	if e.currentMatchIndex < 0 {
		e.currentMatchIndex = len(e.searchMatches) - 1
	}

	// Update saved editor state's cursor to the match
	match := e.searchMatches[e.currentMatchIndex]
	e.savedEditorState.cursor.x = match.x
	e.savedEditorState.cursor.y = match.y

	// Center the match in the view
	visibleLines := e.getVisibleLines()
	e.savedEditorState.scroll.y = match.y - visibleLines/2
	maxScroll := max(0, e.savedEditorState.buffer.LineCount()-visibleLines)
	e.savedEditorState.scroll.y = clamp(e.savedEditorState.scroll.y, 0, maxScroll)
}

func (e *Editor) handleOverlayKey(sym sdl.Keycode) bool {
	switch sym {
	case sdl.K_ESCAPE:
		e.closeOverlay()
		return true
	case sdl.K_RETURN:
		e.executeOverlay()
		return true
	case sdl.K_BACKSPACE:
		if len(e.overlayText) > 0 {
			runes := []rune(e.overlayText)
			e.overlayText = string(runes[:len(runes)-1])
		}
		return true
	}
	return false
}

func (e *Editor) handleEvent(event sdl.Event) {
	switch t := event.(type) {
	case *sdl.QuitEvent:
		e.running = false
	case *sdl.WindowEvent:
		if t.Event == sdl.WINDOWEVENT_RESIZED {
			e.windowWidth = int32(t.Data1)
			e.windowHeight = int32(t.Data2)
		}
	case *sdl.KeyboardEvent:
		if t.Type == sdl.KEYDOWN {
			if e.overlayActive {
				e.handleOverlayKey(t.Keysym.Sym)
				return
			}
			ctrl := (t.Keysym.Mod & sdl.KMOD_CTRL) != 0
			shift := (t.Keysym.Mod & sdl.KMOD_SHIFT) != 0
			alt := (t.Keysym.Mod & sdl.KMOD_ALT) != 0

			// Handle Ctrl shortcuts
			if ctrl && !shift {
				switch t.Keysym.Sym {
				case sdl.K_a:
					e.selectAll()
					return
				case sdl.K_c:
					e.copy()
					return
				case sdl.K_x:
					// Cut: copy then delete selection
					if e.hasSelection() {
						e.copy()
						e.deleteSelection()
					}
					return
				case sdl.K_v:
					e.paste()
					e.ensureCursorVisible()
					return
				case sdl.K_z:
					e.undo()
					e.ensureCursorVisible()
					return
				case sdl.K_y:
					e.redo()
					e.ensureCursorVisible()
					return
				case sdl.K_s:
					if e.searchActive {
						return
					}
					if err := e.saveFile(); err != nil {
						fmt.Fprintf(os.Stderr, "Failed to save file: %v\n", err)
					}
					return
				case sdl.K_o:
					e.beginOverlay(overlayOpen, "Open file", "")
					return
				case sdl.K_n:
					e.newUntitled()
					return
				case sdl.K_g:
					e.beginOverlay(overlayGoto, "Go to line", "")
					return
				case sdl.K_p:
					e.beginOverlay(overlayFilter, "Filter files", e.fileFilter)
					return
				case sdl.K_d:
					e.duplicateLine()
					return
				case sdl.K_r:
					e.buildFileTree()
					return
				case sdl.K_w:
					e.closeCurrentTab()
					return
				case sdl.K_LEFTBRACKET:
					e.unindentSelection("\t")
					return
				case sdl.K_RIGHTBRACKET:
					e.indentSelection("\t")
					return
				case sdl.K_f:
					// Activate search
					e.activateSearch()
					return
				}
			}
			if ctrl && shift {
				switch t.Keysym.Sym {
				case sdl.K_p:
					e.beginOverlay(overlayCommand, "Command", "")
					return
				case sdl.K_n:
					e.beginOverlay(overlayCreate, "Create file", "")
					return
				case sdl.K_s:
					e.beginOverlay(overlaySaveAs, "Save as", e.currentFile)
					return
				case sdl.K_k:
					e.deleteLine()
					return
				case sdl.K_h:
					e.beginOverlay(overlayReplace, "Replace current match with", e.searchReplace)
					return
				}
			}
			if ctrl && t.Keysym.Sym == sdl.K_TAB {
				if shift {
					e.switchTab(-1)
				} else {
					e.switchTab(1)
				}
				return
			}
			if alt {
				switch t.Keysym.Sym {
				case sdl.K_UP:
					e.moveLine(-1)
					return
				case sdl.K_DOWN:
					e.moveLine(1)
					return
				case sdl.K_c:
					if e.searchActive {
						e.searchCaseSensitive = !e.searchCaseSensitive
						e.updateSearchMatches()
					}
					return
				case sdl.K_w:
					if e.searchActive {
						e.searchWholeWord = !e.searchWholeWord
						e.updateSearchMatches()
					}
					return
				case sdl.K_r:
					if e.searchActive {
						e.searchRegex = !e.searchRegex
						e.updateSearchMatches()
					}
					return
				}
			}

			// Handle F3 (Find / Find Next)
			if t.Keysym.Sym == sdl.K_F3 && !ctrl && !shift {
				if !e.searchActive {
					// Activate search if not already active
					e.activateSearch()
				} else if len(e.searchMatches) > 0 {
					// Jump to next match if we have matches
					e.jumpToNextMatch()
				}
				return
			}
			if t.Keysym.Sym == sdl.K_F3 && shift && e.searchActive {
				e.jumpToPreviousMatch()
				return
			}

			// Handle Escape (Close search)
			if t.Keysym.Sym == sdl.K_ESCAPE {
				if e.searchActive {
					e.deactivateSearch()
					return
				}
			}

			// Block certain operations when search is active
			if e.searchActive {
				// Block save, newline, up/down (single line only)
				if ctrl && t.Keysym.Sym == sdl.K_s {
					return
				}
				if t.Keysym.Sym == sdl.K_RETURN {
					// Jump to next match instead of newline
					if len(e.searchMatches) > 0 {
						e.jumpToNextMatch()
					}
					return
				}
				if t.Keysym.Sym == sdl.K_UP || t.Keysym.Sym == sdl.K_DOWN {
					// Block up/down in search box (single line)
					return
				}
				if t.Keysym.Sym == sdl.K_TAB {
					// Block tab in search box
					return
				}
				// Let all other keyboard input fall through to normal handling
				// This allows cursor movement, selection, copy/paste, undo, etc.
			}

			// Start selection if shift is pressed
			if shift && !e.hasSelection() {
				e.selectionStart = &Position{e.cursorX, e.cursorY}
			}

			switch t.Keysym.Sym {
			case sdl.K_BACKSPACE:
				if e.hasSelection() {
					e.deleteSelection()
				} else {
					e.backspace()
				}
				e.ensureCursorVisible()
			case sdl.K_DELETE:
				if e.hasSelection() {
					e.deleteSelection()
				} else {
					e.deleteForward()
				}
			case sdl.K_HOME:
				if !shift {
					e.moveToSelectionStart()
				}
				e.cursorX = 0
				if !shift {
					e.clearSelection()
				}
				e.idealCursorX = -1
				e.ensureCursorVisible()
			case sdl.K_END:
				if !shift {
					e.moveToSelectionEnd()
				}
				e.cursorX = len(e.line(e.cursorY))
				if !shift {
					e.clearSelection()
				}
				e.idealCursorX = -1
				e.ensureCursorVisible()
			case sdl.K_RETURN:
				if e.hasSelection() {
					e.deleteSelection()
				}
				e.newline()
				e.ensureCursorVisible()
			case sdl.K_LEFT:
				if !shift {
					e.moveToSelectionStart()
				}
				// Then perform the movement
				if ctrl {
					e.moveWordLeft()
				} else {
					if e.cursorX > 0 {
						e.cursorX--
					} else if e.cursorY > 0 {
						e.cursorY--
						e.cursorX = len(e.line(e.cursorY))
					}
				}
				e.idealCursorX = -1 // Reset ideal X on horizontal movement
				if !shift {
					e.clearSelection()
				}
				e.ensureCursorVisible()
			case sdl.K_RIGHT:
				if !shift {
					e.moveToSelectionEnd()
				}
				// Then perform the movement
				if ctrl {
					e.moveWordRight()
				} else {
					if e.cursorX < len(e.line(e.cursorY)) {
						e.cursorX++
					} else if e.cursorY < e.lineCount()-1 {
						e.cursorY++
						e.cursorX = 0
					}
				}
				e.idealCursorX = -1 // Reset ideal X on horizontal movement
				if !shift {
					e.clearSelection()
				}
				e.ensureCursorVisible()
			case sdl.K_UP:
				if !shift {
					e.moveToSelectionStart()
				}
				// Calculate ideal pixel X if not set
				if e.idealCursorX == -1 {
					e.idealCursorX = e.pixelWidthBefore(e.line(e.cursorY), e.cursorX)
				}
				// Then perform the movement
				if e.cursorY > 0 {
					e.cursorY--
					e.cursorX = e.findCursorXFromPixel(e.cursorY, e.idealCursorX)
				}
				if !shift {
					e.clearSelection()
				}
				e.ensureCursorVisible()
			case sdl.K_DOWN:
				if !shift {
					e.moveToSelectionEnd()
				}
				// Calculate ideal pixel X if not set
				if e.idealCursorX == -1 {
					e.idealCursorX = e.pixelWidthBefore(e.line(e.cursorY), e.cursorX)
				}
				// Then perform the movement
				if e.cursorY < e.lineCount()-1 {
					e.cursorY++
					e.cursorX = e.findCursorXFromPixel(e.cursorY, e.idealCursorX)
				}
				if !shift {
					e.clearSelection()
				}
				e.ensureCursorVisible()
			case sdl.K_TAB:
				if e.hasSelection() {
					e.deleteSelection()
				}
				e.insertRune('\t')
				e.ensureCursorVisible()
			}

			// Update selection end if shift is still pressed
			if shift && e.selectionStart != nil {
				e.selectionEnd = &Position{e.cursorX, e.cursorY}
			}
		}
	case *sdl.TextInputEvent:
		textLength := 0
		for textLength < len(t.Text) && t.Text[textLength] != 0 {
			textLength++
		}
		text := string(t.Text[:textLength])
		if e.overlayActive {
			e.overlayText += text
			return
		}

		if e.hasSelection() {
			e.deleteSelection()
		}

		for _, r := range text {
			e.insertRune(r)
		}
		e.ensureCursorVisible()
	case *sdl.MouseButtonEvent:
		if t.Type == sdl.MOUSEBUTTONDOWN && t.Button == sdl.BUTTON_LEFT {
			if d := int(t.X) - e.fileBrowserWidth; d >= -4 && d <= 4 {
				e.fileBrowserResizing = true
				return
			}
			if t.X > int32(e.fileBrowserWidth) && t.Y < tabBarHeight {
				e.handleTabClick(t.X)
				return
			}
			// Check if click is on scrollbar
			visibleLines := e.getVisibleLines()
			totalLines := e.lineCount()
			if totalLines > visibleLines {
				scrollbarWidth := int32(8)
				scrollbarX := e.windowWidth - scrollbarWidth - 2

				if t.X >= scrollbarX && t.X <= scrollbarX+scrollbarWidth {
					e.scrollbarDragging = true
					// Calculate scroll position from click
					clickRatio := float64(t.Y) / float64(e.windowHeight)
					e.scrollOffsetY = int(clickRatio * float64(totalLines-visibleLines))

					// Clamp
					maxScroll := max(0, totalLines-visibleLines)
					e.scrollOffsetY = clamp(e.scrollOffsetY, 0, maxScroll)
				} else if t.X < int32(e.fileBrowserWidth) {
					// Check if click is in file browser
					e.handleFileBrowserClick(t.X, t.Y)
				} else {
					// Click in code area - detect double/triple clicks
					currentTime := t.Timestamp
					const doubleClickThreshold = 500 // milliseconds

					// Get the character position of this click
					clickCursorX, clickCursorY := e.getCursorFromMouse(t.X, t.Y)

					// Check if this is a multi-click (double or triple)
					timeDiff := currentTime - e.lastClickTime
					samePosition := (clickCursorX == e.lastClickCursorX && clickCursorY == e.lastClickCursorY)

					if timeDiff < doubleClickThreshold && samePosition {
						// Same character within time threshold - increment click count
						e.clickCount++
						if e.clickCount > 3 {
							e.clickCount = 1
						}
					} else {
						// Different character or too much time - reset to single click
						e.clickCount = 1
					}

					e.lastClickTime = currentTime
					e.lastClickCursorX = clickCursorX
					e.lastClickCursorY = clickCursorY

					e.cursorX = clickCursorX
					e.cursorY = clickCursorY
					e.idealCursorX = -1

					// Handle selection based on click count
					if e.clickCount == 1 {
						// Single click - normal behavior
						e.mouseDown = true
						e.selectionStart = &Position{e.cursorX, e.cursorY}
						e.selectionEnd = nil
					} else if e.clickCount == 2 {
						// Double click - select word (no dragging)
						e.mouseDown = false
						e.selectWord()
					} else if e.clickCount == 3 {
						// Triple click - select line (no dragging)
						e.mouseDown = false
						e.selectLine()
					}

					e.ensureCursorVisible()
				}
			} else if t.X < int32(e.fileBrowserWidth) {
				// Check if click is in file browser
				e.handleFileBrowserClick(t.X, t.Y)
			} else {
				// Click in code area - detect double/triple clicks
				currentTime := t.Timestamp
				const doubleClickThreshold = 500 // milliseconds

				// Get the character position of this click
				clickCursorX, clickCursorY := e.getCursorFromMouse(t.X, t.Y)

				// Check if this is a multi-click (double or triple)
				timeDiff := currentTime - e.lastClickTime
				samePosition := (clickCursorX == e.lastClickCursorX && clickCursorY == e.lastClickCursorY)

				if timeDiff < doubleClickThreshold && samePosition {
					// Same character within time threshold - increment click count
					e.clickCount++
					if e.clickCount > 3 {
						e.clickCount = 1
					}
				} else {
					// Different character or too much time - reset to single click
					e.clickCount = 1
				}

				e.lastClickTime = currentTime
				e.lastClickCursorX = clickCursorX
				e.lastClickCursorY = clickCursorY

				e.cursorX = clickCursorX
				e.cursorY = clickCursorY
				e.idealCursorX = -1

				// Handle selection based on click count
				if e.clickCount == 1 {
					// Single click - normal behavior
					e.mouseDown = true
					e.selectionStart = &Position{e.cursorX, e.cursorY}
					e.selectionEnd = nil
				} else if e.clickCount == 2 {
					// Double click - select word (no dragging)
					e.mouseDown = false
					e.selectWord()
				} else if e.clickCount == 3 {
					// Triple click - select line (no dragging)
					e.mouseDown = false
					e.selectLine()
				}

				e.ensureCursorVisible()
			}
		} else if t.Type == sdl.MOUSEBUTTONUP && t.Button == sdl.BUTTON_LEFT {
			e.mouseDown = false
			e.scrollbarDragging = false
			e.fileBrowserResizing = false
			if e.selectionStart != nil && e.selectionEnd == nil {
				// Just a click, no drag
				e.clearSelection()
			}
		}
	case *sdl.MouseMotionEvent:
		if e.fileBrowserResizing {
			e.fileBrowserWidth = clamp(int(t.X), 120, min(600, int(e.windowWidth)-200))
			e.invalidateFileBrowserWidth()
			return
		}
		// Update cursor appearance based on mouse position
		scrollbarWidth := int32(8)
		scrollbarX := e.windowWidth - scrollbarWidth - 2

		if t.X >= int32(e.fileBrowserWidth+gutterWidth) && t.X < scrollbarX && (e.currentFile != "" || e.untitled) {
			// Mouse is over code area - show I-beam cursor
			sdl.SetCursor(e.ibeamCursor)
		} else {
			// Mouse is over file browser, gutter, or scrollbar - show arrow cursor
			sdl.SetCursor(e.arrowCursor)
		}

		if e.scrollbarDragging {
			// Update scroll position based on mouse Y
			visibleLines := e.getVisibleLines()
			totalLines := e.lineCount()
			clickRatio := float64(t.Y) / float64(e.windowHeight)
			e.scrollOffsetY = int(clickRatio * float64(totalLines-visibleLines))

			// Clamp
			maxScroll := max(0, totalLines-visibleLines)
			e.scrollOffsetY = clamp(e.scrollOffsetY, 0, maxScroll)
		} else if e.mouseDown {
			e.cursorX, e.cursorY = e.getCursorFromMouse(t.X, t.Y)
			e.idealCursorX = -1
			if e.selectionStart != nil {
				e.selectionEnd = &Position{e.cursorX, e.cursorY}
			}
			e.ensureCursorVisible()
		}
	case *sdl.MouseWheelEvent:
		// Get mouse position to determine which area to scroll
		mouseX, _, _ := sdl.GetMouseState()

		if mouseX < int32(e.fileBrowserWidth) {
			// Scroll file browser
			// Vertical scroll
			if t.Y > 0 {
				e.fileBrowserScroll -= 1
			} else if t.Y < 0 {
				e.fileBrowserScroll += 1
			}
			// Clamp file browser vertical scroll
			maxFileBrowserScroll := len(e.flatFileList) - (int(e.windowHeight) / lineHeight)
			if maxFileBrowserScroll < 0 {
				maxFileBrowserScroll = 0
			}
			if e.fileBrowserScroll < 0 {
				e.fileBrowserScroll = 0
			}
			if e.fileBrowserScroll > maxFileBrowserScroll {
				e.fileBrowserScroll = maxFileBrowserScroll
			}

			// Horizontal scroll (for trackpoints and mice with horizontal wheel)
			if t.X != 0 {
				scrollAmount := 30 // Pixels per tick
				if t.X > 0 {
					e.fileBrowserScrollX += scrollAmount // Scroll right
				} else if t.X < 0 {
					e.fileBrowserScrollX -= scrollAmount // Scroll left
				}

				maxWidth := e.getFileBrowserMaxWidth()

				// Clamp horizontal scroll
				maxFileBrowserScrollX := maxWidth - e.fileBrowserWidth + 20
				if maxFileBrowserScrollX < 0 {
					maxFileBrowserScrollX = 0
				}
				if e.fileBrowserScrollX < 0 {
					e.fileBrowserScrollX = 0
				}
				if e.fileBrowserScrollX > maxFileBrowserScrollX {
					e.fileBrowserScrollX = maxFileBrowserScrollX
				}
			}
		} else {
			// Scroll code area
			// Vertical scroll
			if t.Y > 0 {
				e.scroll(-1) // Scroll up
			} else if t.Y < 0 {
				e.scroll(1) // Scroll down
			}

			// Horizontal scroll (for trackpoints and mice with horizontal wheel)
			if t.X != 0 {
				scrollAmount := 30 // Pixels per tick
				if t.X > 0 {
					e.scrollOffsetX += scrollAmount // Scroll right
				} else if t.X < 0 {
					e.scrollOffsetX -= scrollAmount // Scroll left
				}

				// Use cached maximum line width
				maxWidth := e.getMaxLineWidth()

				// Calculate visible code area width
				codeAreaWidth := int(e.windowWidth) - e.fileBrowserWidth - gutterWidth
				maxScrollX := maxWidth - codeAreaWidth + 20 // +20 for padding
				if maxScrollX < 0 {
					maxScrollX = 0
				}

				// Clamp horizontal scroll
				if e.scrollOffsetX < 0 {
					e.scrollOffsetX = 0
				}
				if e.scrollOffsetX > maxScrollX {
					e.scrollOffsetX = maxScrollX
				}
			}
		}
		// Don't call ensureCursorVisible here - allow free scrolling
	case *sdl.DropEvent:
		if t.Type == sdl.DROPFILE && t.File != "" {
			info, err := os.Stat(t.File)
			if err == nil && info.IsDir() {
				e.rootDir = t.File
				e.stateDir = filepath.Join(e.rootDir, ".rune")
				_ = os.MkdirAll(filepath.Join(e.stateDir, "autosave"), 0755)
				e.openFiles = nil
				e.fileStates = make(map[string]*FileState)
				e.currentFile = ""
				e.untitled = false
				e.buffer = NewPieceTable("")
				e.buildFileTree()
				e.saveSession()
			} else if err == nil {
				_ = e.loadFile(t.File)
			}
		}
	}
}

func (e *Editor) handleTabClick(x int32) {
	tabX := int32(e.fileBrowserWidth + 4)
	for _, path := range e.openFiles {
		name := filepath.Base(path)
		if e.fileDirty(path) {
			name += " *"
		}
		w := int32(max(80, len([]rune(name))*e.charWidth+18))
		if x >= tabX && x <= tabX+w {
			_ = e.loadFile(path)
			return
		}
		tabX += w + 3
	}
}

func keywordColor(word string) sdl.Color {
	switch word {
	case "break", "case", "catch", "class", "const", "continue", "def", "default", "defer", "del", "else", "enum", "export", "extends", "false", "finally", "for", "func", "function", "go", "if", "import", "in", "interface", "let", "map", "match", "new", "nil", "null", "package", "pass", "return", "struct", "switch", "this", "throw", "true", "try", "type", "var", "while":
		return sdl.Color{R: 110, G: 170, B: 230, A: 255}
	}
	return sdl.Color{R: 200, G: 200, B: 200, A: 255}
}

func (e *Editor) renderHighlightedLine(line []rune, x, y int32, scrollX int) {
	col := int32(0)
	for i := 0; i < len(line); {
		start := i
		color := sdl.Color{R: 200, G: 200, B: 200, A: 255}
		switch {
		case i+1 < len(line) && line[i] == '/' && line[i+1] == '/':
			i = len(line)
			color = sdl.Color{R: 120, G: 150, B: 120, A: 255}
		case line[i] == '#':
			i = len(line)
			color = sdl.Color{R: 120, G: 150, B: 120, A: 255}
		case line[i] == '"' || line[i] == '\'':
			quote := line[i]
			i++
			for i < len(line) {
				if line[i] == '\\' && i+1 < len(line) {
					i += 2
					continue
				}
				if line[i] == quote {
					i++
					break
				}
				i++
			}
			color = sdl.Color{R: 210, G: 170, B: 110, A: 255}
		case unicode.IsDigit(line[i]):
			for i < len(line) && (unicode.IsDigit(line[i]) || line[i] == '.') {
				i++
			}
			color = sdl.Color{R: 190, G: 150, B: 220, A: 255}
		case isWordChar(line[i]):
			for i < len(line) && isWordChar(line[i]) {
				i++
			}
			color = keywordColor(string(line[start:i]))
		default:
			i++
		}
		text := expandTabsForDisplay(line[start:i], tabWidth)
		if text == "" {
			continue
		}
		cached, err := e.cachedText(text, color)
		if err == nil {
			dstX := x + col - int32(scrollX)
			if dstX+cached.w > x {
				e.renderer.Copy(cached.texture, nil, &sdl.Rect{X: dstX, Y: y, W: cached.w, H: cached.h})
			}
			col += int32(displayColumn(line[start:i], len(line[start:i])) * e.charWidth)
		}
	}
}

func (e *Editor) renderTabBar() {
	x := int32(e.fileBrowserWidth)
	e.renderer.SetDrawColor(22, 22, 22, 255)
	e.renderer.FillRect(&sdl.Rect{X: x, Y: 0, W: e.windowWidth - x, H: tabBarHeight})
	tabX := x + 4
	for _, path := range e.openFiles {
		name := filepath.Base(path)
		if e.fileDirty(path) {
			name += " *"
		}
		active := path == e.currentFile
		if active {
			e.renderer.SetDrawColor(45, 55, 65, 255)
		} else {
			e.renderer.SetDrawColor(30, 30, 30, 255)
		}
		w := int32(max(80, len([]rune(name))*e.charWidth+18))
		e.renderer.FillRect(&sdl.Rect{X: tabX, Y: 2, W: w, H: tabBarHeight - 4})
		cached, err := e.cachedText(name, sdl.Color{R: 190, G: 190, B: 190, A: 255})
		if err == nil {
			e.renderer.Copy(cached.texture, nil, &sdl.Rect{X: tabX + 8, Y: 4, W: cached.w, H: cached.h})
		}
		tabX += w + 3
	}
	if e.untitled {
		name := e.untitledName + " *"
		e.renderer.SetDrawColor(45, 55, 65, 255)
		w := int32(max(90, len([]rune(name))*e.charWidth+18))
		e.renderer.FillRect(&sdl.Rect{X: tabX, Y: 2, W: w, H: tabBarHeight - 4})
		cached, err := e.cachedText(name, sdl.Color{R: 190, G: 190, B: 190, A: 255})
		if err == nil {
			e.renderer.Copy(cached.texture, nil, &sdl.Rect{X: tabX + 8, Y: 4, W: cached.w, H: cached.h})
		}
	}
}

func (e *Editor) renderOverlay() {
	if !e.overlayActive {
		return
	}
	w := int32(520)
	h := int32(96)
	x := (e.windowWidth - w) / 2
	y := int32(48)
	e.renderer.SetDrawColor(28, 28, 28, 245)
	e.renderer.FillRect(&sdl.Rect{X: x, Y: y, W: w, H: h})
	e.renderer.SetDrawColor(90, 90, 90, 255)
	e.renderer.DrawRect(&sdl.Rect{X: x, Y: y, W: w, H: h})
	if cached, err := e.cachedText(e.overlayTitle, sdl.Color{R: 180, G: 180, B: 180, A: 255}); err == nil {
		e.renderer.Copy(cached.texture, nil, &sdl.Rect{X: x + 12, Y: y + 10, W: cached.w, H: cached.h})
	}
	input := e.overlayText
	if input == "" {
		input = " "
	}
	if cached, err := e.cachedText(input, sdl.Color{R: 235, G: 235, B: 235, A: 255}); err == nil {
		e.renderer.Copy(cached.texture, nil, &sdl.Rect{X: x + 12, Y: y + 42, W: cached.w, H: cached.h})
		cursorX := x + 12 + int32(len([]rune(e.overlayText))*e.charWidth)
		e.renderer.SetDrawColor(235, 235, 235, 255)
		e.renderer.FillRect(&sdl.Rect{X: cursorX, Y: y + 40, W: 2, H: lineHeight})
	}
	if e.overlayMode == overlayCommand {
		hint := "new open create rename delete goto find replace replace all filter refresh save save as next tab previous tab close tab"
		if cached, err := e.cachedText(hint, sdl.Color{R: 120, G: 120, B: 120, A: 255}); err == nil {
			e.renderer.Copy(cached.texture, nil, &sdl.Rect{X: x + 12, Y: y + 70, W: cached.w, H: cached.h})
		}
	}
}

// renderSearchBox renders the search box at the top of the code area
func (e *Editor) renderSearchBox() {
	if !e.searchActive {
		return
	}

	searchBoxHeight := int32(30)
	searchBoxY := int32(tabBarHeight)
	codeAreaX := int32(e.fileBrowserWidth)

	// Render search box background
	e.renderer.SetDrawColor(30, 30, 30, 255)
	e.renderer.FillRect(&sdl.Rect{
		X: codeAreaX,
		Y: searchBoxY,
		W: e.windowWidth - codeAreaX,
		H: searchBoxHeight,
	})

	// Render border
	e.renderer.SetDrawColor(60, 60, 60, 255)
	e.renderer.DrawRect(&sdl.Rect{
		X: codeAreaX,
		Y: searchBoxY,
		W: e.windowWidth - codeAreaX,
		H: searchBoxHeight,
	})

	// Render "Find:" label
	labelCached, err := e.cachedText("Find:", sdl.Color{R: 180, G: 180, B: 180, A: 255})
	if err == nil {
		labelRect := &sdl.Rect{
			X: codeAreaX + 10,
			Y: searchBoxY + (searchBoxHeight-labelCached.h)/2,
			W: labelCached.w,
			H: labelCached.h,
		}
		e.renderer.Copy(labelCached.texture, nil, labelRect)
	}

	// Render search query from the search piece table.
	queryX := codeAreaX + 60
	searchQuery := e.buffer.Line(0)
	queryText := string(searchQuery)
	if queryText == "" {
		queryText = " " // Render space to show cursor
	}

	// Render selection if any
	if e.hasSelection() {
		start, end := e.getSelectionBounds()
		if start.y == 0 && end.y == 0 {
			startW := e.pixelWidthBefore(searchQuery, start.x)
			selectedW := e.pixelWidthBefore(searchQuery, end.x) - startW
			e.renderer.SetDrawColor(50, 80, 120, 255)
			e.renderer.FillRect(&sdl.Rect{
				X: queryX + int32(startW),
				Y: searchBoxY + 5,
				W: int32(selectedW),
				H: searchBoxHeight - 10,
			})
		}
	}

	queryCached, err := e.cachedText(queryText, sdl.Color{R: 255, G: 255, B: 255, A: 255})
	if err == nil {
		queryRect := &sdl.Rect{
			X: queryX,
			Y: searchBoxY + (searchBoxHeight-queryCached.h)/2,
			W: queryCached.w,
			H: queryCached.h,
		}
		e.renderer.Copy(queryCached.texture, nil, queryRect)

		// Render cursor at current cursor position
		cursorX := queryX
		if e.cursorX > 0 {
			cursorX = queryX + int32(e.pixelWidthBefore(searchQuery, e.cursorX))
		}
		e.renderer.SetDrawColor(255, 255, 255, 255)
		e.renderer.FillRect(&sdl.Rect{
			X: cursorX,
			Y: searchBoxY + 5,
			W: 2,
			H: searchBoxHeight - 10,
		})
	}

	// Render match count
	if len(searchQuery) > 0 {
		matchText := ""
		if len(e.searchMatches) == 0 {
			matchText = "No matches"
		} else {
			matchText = fmt.Sprintf("%d of %d", e.currentMatchIndex+1, len(e.searchMatches))
		}
		flags := []string{}
		if e.searchCaseSensitive {
			flags = append(flags, "case")
		}
		if e.searchWholeWord {
			flags = append(flags, "word")
		}
		if e.searchRegex {
			flags = append(flags, "regex")
		}
		if len(flags) > 0 {
			matchText += "  " + strings.Join(flags, " ")
		}
		matchCached, err := e.cachedText(matchText, sdl.Color{R: 150, G: 150, B: 150, A: 255})
		if err == nil {
			matchRect := &sdl.Rect{
				X: e.windowWidth - matchCached.w - 20,
				Y: searchBoxY + (searchBoxHeight-matchCached.h)/2,
				W: matchCached.w,
				H: matchCached.h,
			}
			e.renderer.Copy(matchCached.texture, nil, matchRect)
		}
	}
}

func (e *Editor) renderWelcomeScreen() {
	// Render file browser
	e.renderFileBrowser()

	// Render welcome message in the center of the code area
	welcomeMsg := "Welcome to " + programName + "!"
	cached, err := e.cachedText(welcomeMsg, sdl.Color{R: 150, G: 150, B: 150, A: 255})
	if err == nil {
		// Center the text in the code area
		codeAreaWidth := e.windowWidth - int32(e.fileBrowserWidth)
		textX := int32(e.fileBrowserWidth) + (codeAreaWidth-cached.w)/2
		textY := (e.windowHeight - cached.h) / 2

		rect := &sdl.Rect{X: textX, Y: textY, W: cached.w, H: cached.h}
		e.renderer.Copy(cached.texture, nil, rect)
	}
}

func (e *Editor) render() {
	// Update window title
	title := programName
	if e.currentFile != "" {
		fileName := filepath.Base(e.currentFile)
		title = fileName + " | " + programName
	} else if e.untitled {
		title = e.untitledName + " | " + programName
	}
	e.window.SetTitle(title)

	e.renderer.SetDrawColor(15, 15, 15, 255)
	e.renderer.Clear()

	codeAreaX := int32(e.fileBrowserWidth)
	searchBoxHeight := int32(0)
	if e.searchActive {
		searchBoxHeight = 30
	}
	codeTop := int32(tabBarHeight) + searchBoxHeight

	// If no file is open, show welcome screen
	if e.currentFile == "" && !e.untitled {
		e.renderWelcomeScreen()
		e.renderOverlay()
		e.renderer.Present()
		return
	}

	// Use saved editor state for rendering when search is active
	var renderBuffer *PieceTable
	var renderScrollY int
	var renderScrollX int
	if e.searchActive && e.savedEditorState != nil {
		renderBuffer = e.savedEditorState.buffer
		renderScrollY = e.savedEditorState.scroll.y
		renderScrollX = e.savedEditorState.scroll.x
	} else {
		renderBuffer = e.buffer
		renderScrollY = e.scrollOffsetY
		renderScrollX = e.scrollOffsetX
	}

	visibleLines := e.getVisibleLines()
	startLine := renderScrollY
	endLine := renderScrollY + visibleLines
	if endLine > renderBuffer.LineCount() {
		endLine = renderBuffer.LineCount()
	}

	// Render file browser
	e.renderFileBrowser()
	e.renderTabBar()

	// Render current line highlight (only for saved state when searching, not for search box)
	if e.searchActive && e.savedEditorState != nil {
		cursorY := e.savedEditorState.cursor.y
		if cursorY >= startLine && cursorY < endLine {
			screenY := (cursorY - renderScrollY) * lineHeight
			e.renderer.SetDrawColor(40, 40, 40, 255)
			e.renderer.FillRect(&sdl.Rect{
				X: codeAreaX,
				Y: int32(screenY) + codeTop,
				W: e.windowWidth - codeAreaX,
				H: lineHeight,
			})
		}
	} else if !e.searchActive && e.cursorY >= startLine && e.cursorY < endLine {
		screenY := (e.cursorY - renderScrollY) * lineHeight
		e.renderer.SetDrawColor(40, 40, 40, 255)
		e.renderer.FillRect(&sdl.Rect{
			X: codeAreaX,
			Y: int32(screenY) + codeTop,
			W: e.windowWidth - codeAreaX,
			H: lineHeight,
		})
	}

	// Set clip rect for code area to prevent overflow (below search box)
	codeClipRect := &sdl.Rect{
		X: codeAreaX,
		Y: codeTop,
		W: e.windowWidth - codeAreaX,
		H: e.windowHeight - codeTop,
	}
	e.renderer.SetClipRect(codeClipRect)

	// Render line numbers (only visible lines)
	for y := startLine; y < endLine; y++ {
		lineNum := fmt.Sprintf("%d", y+1)
		cached, err := e.cachedText(lineNum, sdl.Color{R: 100, G: 100, B: 100, A: 255})
		if err != nil {
			continue
		}

		screenY := (y - renderScrollY) * lineHeight
		rect := &sdl.Rect{X: codeAreaX + int32(gutterWidth-cached.w-10), Y: int32(screenY) + codeTop, W: cached.w, H: cached.h}
		e.renderer.Copy(cached.texture, nil, rect)
	}

	// Render gutter separator
	e.renderer.SetDrawColor(50, 50, 50, 255)
	e.renderer.DrawLine(codeAreaX+gutterWidth-5, codeTop, codeAreaX+gutterWidth-5, e.windowHeight)

	// Update clip rect to exclude gutter - only clip text area
	textAreaClipRect := &sdl.Rect{
		X: codeAreaX + gutterWidth,
		Y: codeTop,
		W: e.windowWidth - codeAreaX - gutterWidth,
		H: e.windowHeight - codeTop,
	}
	e.renderer.SetClipRect(textAreaClipRect)

	// Render selection (only when not searching, since search selection is in search box)
	if !e.searchActive && e.hasSelection() {
		start, end := e.selectionStart, e.selectionEnd
		if start.y > end.y || (start.y == end.y && start.x > end.x) {
			start, end = end, start
		}

		e.renderer.SetDrawColor(50, 80, 120, 255)

		if start.y == end.y {
			// Single line selection
			if start.y >= startLine && start.y < endLine {
				line := renderBuffer.Line(start.y)
				startW := e.pixelWidthBefore(line, start.x)
				selectedW := e.pixelWidthBefore(line, end.x) - startW
				screenY := (start.y - renderScrollY) * lineHeight
				e.renderer.FillRect(&sdl.Rect{
					X: codeAreaX + int32(gutterWidth+startW-renderScrollX),
					Y: int32(screenY) + codeTop,
					W: int32(selectedW),
					H: lineHeight,
				})
			}
		} else {
			// Multi-line selection
			// First line
			if start.y >= startLine && start.y < endLine {
				line := renderBuffer.Line(start.y)
				startW := e.pixelWidthBefore(line, start.x)
				restW := e.pixelWidthForRunes(line) - startW
				screenY := (start.y - renderScrollY) * lineHeight
				e.renderer.FillRect(&sdl.Rect{
					X: codeAreaX + int32(gutterWidth+startW-renderScrollX),
					Y: int32(screenY) + codeTop,
					W: int32(restW),
					H: lineHeight,
				})
			}

			// Middle lines
			for y := start.y + 1; y < end.y; y++ {
				if y >= startLine && y < endLine {
					w := e.pixelWidthForRunes(renderBuffer.Line(y))
					screenY := (y - renderScrollY) * lineHeight
					e.renderer.FillRect(&sdl.Rect{
						X: codeAreaX + int32(gutterWidth-renderScrollX),
						Y: int32(screenY) + codeTop,
						W: int32(w),
						H: lineHeight,
					})
				}
			}

			// Last line
			if end.y >= startLine && end.y < endLine {
				endW := e.pixelWidthBefore(renderBuffer.Line(end.y), end.x)
				screenY := (end.y - renderScrollY) * lineHeight
				e.renderer.FillRect(&sdl.Rect{
					X: codeAreaX + int32(gutterWidth-renderScrollX),
					Y: int32(screenY) + codeTop,
					W: int32(endW),
					H: lineHeight,
				})
			}
		}
	}

	// Render search match highlights
	searchQuery := e.buffer.Line(0)
	if e.searchActive && len(e.searchMatches) > 0 && len(searchQuery) > 0 {
		for _, matchIndex := range e.visibleSearchMatchIndexes(startLine, endLine) {
			match := e.searchMatches[matchIndex]
			queryLen := len(searchQuery)
			if matchIndex < len(e.searchMatchLens) {
				queryLen = e.searchMatchLens[matchIndex]
			}
			// Only render matches in visible lines
			if match.y >= startLine && match.y < endLine {
				line := renderBuffer.Line(match.y)
				startW := e.pixelWidthBefore(line, match.x)
				matchW := e.pixelWidthBefore(line, match.x+queryLen) - startW
				screenY := (match.y - renderScrollY) * lineHeight

				// Different color for current match vs other matches
				if matchIndex == e.currentMatchIndex {
					// Current match - vibrant bright blue
					e.renderer.SetDrawColor(100, 160, 220, 255)
				} else {
					// Other matches - very subtle dark blue-gray
					e.renderer.SetDrawColor(50, 70, 85, 255)
				}

				e.renderer.FillRect(&sdl.Rect{
					X: codeAreaX + int32(gutterWidth+startW-renderScrollX),
					Y: int32(screenY) + codeTop,
					W: int32(matchW),
					H: lineHeight,
				})
			}
		}
	}

	// Render text (only visible lines)
	for y := startLine; y < endLine; y++ {
		line := renderBuffer.Line(y)
		if len(line) == 0 {
			continue
		}
		screenY := (y - renderScrollY) * lineHeight
		e.renderHighlightedLine(line, codeAreaX+int32(gutterWidth), int32(screenY)+codeTop, renderScrollX)
	}

	// Render cursor in code area (only if not searching)
	if !e.searchActive && e.cursorY >= startLine && e.cursorY < endLine {
		cursorPixelX := 0
		if e.cursorX > 0 {
			cursorPixelX = e.pixelWidthBefore(renderBuffer.Line(e.cursorY), e.cursorX)
		}
		screenY := (e.cursorY - renderScrollY) * lineHeight
		cursorY := screenY + (lineHeight-fontSize)/2
		cursorX := codeAreaX + int32(gutterWidth+cursorPixelX-renderScrollX) - 1

		e.renderer.SetDrawColor(255, 255, 255, 255)
		e.renderer.FillRect(&sdl.Rect{X: cursorX, Y: int32(cursorY) + codeTop, W: 2, H: fontSize})
	}

	// Clear clip rect
	e.renderer.SetClipRect(nil)

	// Render search box on top
	e.renderSearchBox()
	e.renderOverlay()

	// Render scrollbar (only if needed)
	totalLines := renderBuffer.LineCount()
	if totalLines > visibleLines {
		scrollbarWidth := int32(8)
		scrollbarX := e.windowWidth - scrollbarWidth - 2

		// Scrollbar track
		e.renderer.SetDrawColor(30, 30, 30, 255)
		e.renderer.FillRect(&sdl.Rect{X: scrollbarX, Y: 0, W: scrollbarWidth, H: e.windowHeight})

		// Scrollbar thumb
		thumbHeight := (visibleLines * int(e.windowHeight)) / totalLines
		if thumbHeight < 20 {
			thumbHeight = 20
		}
		thumbY := (renderScrollY * (int(e.windowHeight) - thumbHeight)) / (totalLines - visibleLines)

		e.renderer.SetDrawColor(80, 80, 80, 255)
		e.renderer.FillRect(&sdl.Rect{X: scrollbarX, Y: int32(thumbY), W: scrollbarWidth, H: int32(thumbHeight)})
	}

	e.renderer.Present()
}

func (e *Editor) run() {
	sdl.StartTextInput()
	defer sdl.StopTextInput()

	// Initial render
	e.render()

	for e.running {
		// Wait for an event (blocks until event occurs - 0% CPU when idle!)
		event := sdl.WaitEvent()
		if event != nil {
			e.handleEvent(event)

			// Process any additional queued events before rendering
			for event := sdl.PollEvent(); event != nil; event = sdl.PollEvent() {
				e.handleEvent(event)
			}

			// Render once after handling all events
			e.render()
		}
	}
}

func (e *Editor) cleanup() {
	e.saveSession()
	// Note: SDL cursors are freed automatically, no need to destroy them
	for _, cached := range e.textCache {
		if cached.texture != nil {
			cached.texture.Destroy()
		}
	}
	e.font.Close()
	e.renderer.Destroy()
	e.window.Destroy()
	ttf.Quit()
	sdl.Quit()
}

func main() {
	runtime.LockOSThread()

	var targetPath string

	// Parse arguments
	args := os.Args[1:]
	for i, arg := range args {
		if arg != "--detached" && i == len(args)-1 {
			// Last non-flag argument is the target path
			targetPath = arg
		}
	}

	editor, err := NewEditor(targetPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create editor: %v\n", err)
		os.Exit(1)
	}
	defer editor.cleanup()

	editor.run()
}
