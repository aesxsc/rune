package main

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
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
	fileBrowserWidth    = 250
	textCacheLimit      = 2048
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
}

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
	rootDir                  string
	currentFile              string
	fileStates               map[string]*FileState
	isDirty                  bool
	savedAtUndoDepth         int // Undo stack depth when file was saved (-1 = unreachable)
	fileTree                 *FileNode
	nodeByPath               map[string]*FileNode
	flatFileList             []*FileNode
	fileBrowserScroll        int
	fileBrowserScrollX       int
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
	currentMatchIndex        int         // Index of currently highlighted match
	savedEditorState         *FileState  // Saved editor state when search is active
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
		fileStates:               make(map[string]*FileState),
		nodeByPath:               make(map[string]*FileNode),
		isDirty:                  false,
		savedAtUndoDepth:         0,
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
	}
	if w, _, err := font.SizeUTF8("M"); err == nil && w > 0 {
		editor.charWidth = w
	}

	// Build file tree
	editor.buildFileTree()

	return editor, nil
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
		if strings.HasPrefix(entry.Name(), ".") {
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

	e.flatFileList = append(e.flatFileList, node)

	if node.isDir && node.expanded {
		for _, child := range node.children {
			e.flattenNode(child)
		}
	}
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
		if !node.isDir {
			if node.path == e.currentFile && e.isDirty {
				nameLen += 2
			} else if _, exists := e.fileStates[node.path]; exists {
				nameLen += 2
			}
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
	e.renderer.FillRect(&sdl.Rect{X: 0, Y: 0, W: fileBrowserWidth, H: e.windowHeight})

	// Render file browser separator
	e.renderer.SetDrawColor(50, 50, 50, 255)
	e.renderer.DrawLine(fileBrowserWidth, 0, fileBrowserWidth, e.windowHeight)

	// Set clip rect for file browser to prevent overflow
	fileBrowserClipRect := &sdl.Rect{
		X: 0,
		Y: 0,
		W: fileBrowserWidth,
		H: e.windowHeight,
	}
	e.renderer.SetClipRect(fileBrowserClipRect)

	// Get the highlighted node
	highlightedNode := e.getHighlightedNode()

	// Render file browser items
	fileBrowserVisibleLines := int(e.windowHeight) / lineHeight
	for i := e.fileBrowserScroll; i < len(e.flatFileList) && i < e.fileBrowserScroll+fileBrowserVisibleLines; i++ {
		node := e.flatFileList[i]
		y := (i - e.fileBrowserScroll) * lineHeight

		// Highlight if this is the active node
		if highlightedNode != nil && node.path == highlightedNode.path {
			e.renderer.SetDrawColor(40, 60, 80, 255)
			e.renderer.FillRect(&sdl.Rect{
				X: 0,
				Y: int32(y),
				W: fileBrowserWidth,
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
			// Check if it's the current file with unsaved changes
			if node.path == e.currentFile && e.isDirty {
				hasUnsavedChanges = true
			}
			// Check if it's in fileStates (other files with unsaved changes)
			if _, exists := e.fileStates[node.path]; exists {
				hasUnsavedChanges = true
			}
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
		currentLine := e.line(e.cursorY)
		deleteOffset := e.positionOffset(Position{x: len(prevLine), y: e.cursorY - 1})
		// Join with previous line
		oldPrevLine := append([]rune(nil), prevLine...)
		oldCurrentLine := append([]rune(nil), currentLine...)
		joinedLine := append(append([]rune(nil), prevLine...), currentLine...)

		op := UndoOp{
			deleteAt:     deleteOffset - len(prevLine),
			deleted:      append(append(oldPrevLine, '\n'), oldCurrentLine...),
			insertAt:     deleteOffset - len(prevLine),
			inserted:     joinedLine,
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
		nextLine := e.line(e.cursorY + 1)
		deleteOffset := e.positionOffset(Position{x: len(line), y: e.cursorY})
		// Join with next line
		oldCurrentLine := append([]rune(nil), line...)
		oldNextLine := append([]rune(nil), nextLine...)
		joinedLine := append(append([]rune(nil), line...), nextLine...)

		op := UndoOp{
			deleteAt:     deleteOffset - len(line),
			deleted:      append(append(oldCurrentLine, '\n'), oldNextLine...),
			insertAt:     deleteOffset - len(line),
			inserted:     joinedLine,
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
	line := e.line(e.cursorY)

	oldLine := append([]rune(nil), line...)
	firstPart := append([]rune(nil), line[:e.cursorX]...)
	secondPart := append([]rune(nil), line[e.cursorX:]...)

	op := UndoOp{
		deleteAt:     insertOffset - e.cursorX,
		deleted:      oldLine,
		insertAt:     insertOffset - e.cursorX,
		inserted:     append(append(firstPart, '\n'), secondPart...),
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
	lineY := int(y)/lineHeight + e.scrollOffsetY
	if lineY < 0 {
		lineY = 0
	}
	if lineY >= e.lineCount() {
		lineY = e.lineCount() - 1
	}

	// Adjust x for file browser and gutter, and add horizontal scroll
	adjustedX := int(x) - fileBrowserWidth - gutterWidth + e.scrollOffsetX

	if adjustedX < 0 {
		return 0, lineY
	}

	line := e.line(lineY)
	return cursorFromDisplayColumn(line, adjustedX/e.charWidth), lineY
}

func (e *Editor) getVisibleLines() int {
	return int(e.windowHeight) / lineHeight
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
	codeAreaWidth := int(e.windowWidth) - fileBrowserWidth - gutterWidth - 20

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
	codeAreaWidth := int(e.windowWidth) - fileBrowserWidth - gutterWidth - 20

	e.scrollOffsetX = cursorPixelX - codeAreaWidth/2

	e.scrollOffsetX = max(0, e.scrollOffsetX)
}

func (e *Editor) scroll(delta int) {
	e.scrollOffsetY += delta
	visibleLines := e.getVisibleLines()
	maxScroll := max(0, e.lineCount()-visibleLines)
	e.scrollOffsetY = clamp(e.scrollOffsetY, 0, maxScroll)
}

func (e *Editor) saveFile() error {
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
	delete(e.fileStates, e.currentFile)
	e.invalidateFileBrowserWidth()

	return nil
}

func (e *Editor) loadFile(path string) error {
	// Save current file state if we have unsaved changes
	if e.currentFile != "" && e.isDirty {
		state := &FileState{
			buffer:           e.buffer.Clone(),
			cursor:           Position{x: e.cursorX, y: e.cursorY},
			scroll:           Position{x: e.scrollOffsetX, y: e.scrollOffsetY},
			undoStack:        cloneUndoStack(e.undoStack),
			redoStack:        cloneUndoStack(e.redoStack),
			savedAtUndoDepth: e.savedAtUndoDepth,
		}
		e.fileStates[e.currentFile] = state
		e.invalidateFileBrowserWidth()
	}

	// Check if we have unsaved changes for this file
	if savedState, exists := e.fileStates[path]; exists {
		// Restore from saved state
		e.buffer = savedState.buffer.Clone()
		e.cursorX = savedState.cursor.x
		e.cursorY = savedState.cursor.y
		e.scrollOffsetX = savedState.scroll.x
		e.scrollOffsetY = savedState.scroll.y
		e.currentFile = path
		e.isDirty = true
		e.savedAtUndoDepth = savedState.savedAtUndoDepth
		e.clearSelection()
		e.undoStack = cloneUndoStack(savedState.undoStack)
		e.redoStack = cloneUndoStack(savedState.redoStack)
		e.invalidateMaxLineWidth()
		e.invalidateFileBrowserWidth()
		return nil
	}

	// Load from disk
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	// Clear undo/redo stacks
	e.undoStack = nil
	e.redoStack = nil

	// Convert content to string to properly handle UTF-8 multibyte characters
	contentStr := string(content)
	e.buffer = NewPieceTable(contentStr)

	e.currentFile = path
	e.cursorX = 0
	e.cursorY = 0
	e.scrollOffsetX = 0
	e.scrollOffsetY = 0
	e.isDirty = false
	e.savedAtUndoDepth = 0 // Empty undo stack = just loaded state
	e.clearSelection()
	e.invalidateMaxLineWidth()
	e.invalidateFileBrowserWidth()

	return nil
}

func (e *Editor) handleFileBrowserClick(x, y int32) {
	if x >= fileBrowserWidth {
		return
	}

	// Calculate which file was clicked
	clickedLine := int(y) / lineHeight
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

// activateSearch saves current editor state and sets up search mode
func (e *Editor) activateSearch() {
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
	e.currentMatchIndex = -1

	searchQuery := e.buffer.Line(0)
	if len(searchQuery) == 0 {
		return
	}

	// Convert search query to lowercase for case-insensitive search
	queryLower := lowerRunes(searchQuery)
	queryLen := len(searchQuery)

	// Search through saved editor lines
	for y := 0; y < e.savedEditorState.buffer.LineCount(); y++ {
		line := e.savedEditorState.buffer.Line(y)
		if len(line) < queryLen {
			continue
		}
		lineLower := lowerRunes(line)

		// Find all occurrences in this line
		for x := 0; x <= len(line)-queryLen; x++ {
			if runeSliceEqual(lineLower[x:x+queryLen], queryLower) {
				e.searchMatches = append(e.searchMatches, Position{x: x, y: y})
			}
		}
	}

	// Set current match to first result if any found
	if len(e.searchMatches) > 0 {
		e.currentMatchIndex = 0
	}
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
			ctrl := (t.Keysym.Mod & sdl.KMOD_CTRL) != 0
			shift := (t.Keysym.Mod & sdl.KMOD_SHIFT) != 0

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
				case sdl.K_f:
					// Activate search
					if e.currentFile != "" {
						e.activateSearch()
					}
					return
				}
			}

			// Handle F3 (Find / Find Next)
			if t.Keysym.Sym == sdl.K_F3 && !ctrl && !shift {
				if !e.searchActive {
					// Activate search if not already active
					if e.currentFile != "" {
						e.activateSearch()
					}
				} else if len(e.searchMatches) > 0 {
					// Jump to next match if we have matches
					e.jumpToNextMatch()
				}
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

		if e.hasSelection() {
			e.deleteSelection()
		}

		for _, r := range text {
			e.insertRune(r)
		}
		e.ensureCursorVisible()
	case *sdl.MouseButtonEvent:
		if t.Type == sdl.MOUSEBUTTONDOWN && t.Button == sdl.BUTTON_LEFT {
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
				} else if t.X < fileBrowserWidth {
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
			} else if t.X < fileBrowserWidth {
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
			if e.selectionStart != nil && e.selectionEnd == nil {
				// Just a click, no drag
				e.clearSelection()
			}
		}
	case *sdl.MouseMotionEvent:
		// Update cursor appearance based on mouse position
		scrollbarWidth := int32(8)
		scrollbarX := e.windowWidth - scrollbarWidth - 2

		if t.X >= fileBrowserWidth+gutterWidth && t.X < scrollbarX && e.currentFile != "" {
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

		if mouseX < fileBrowserWidth {
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
				maxFileBrowserScrollX := maxWidth - fileBrowserWidth + 20
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
				codeAreaWidth := int(e.windowWidth) - fileBrowserWidth - gutterWidth
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
	}
}

// renderSearchBox renders the search box at the top of the code area
func (e *Editor) renderSearchBox() {
	if !e.searchActive {
		return
	}

	searchBoxHeight := int32(30)
	searchBoxY := int32(0)
	codeAreaX := int32(fileBrowserWidth)

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
		codeAreaWidth := e.windowWidth - fileBrowserWidth
		textX := fileBrowserWidth + (codeAreaWidth-cached.w)/2
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
	}
	e.window.SetTitle(title)

	e.renderer.SetDrawColor(15, 15, 15, 255)
	e.renderer.Clear()

	codeAreaX := int32(fileBrowserWidth)
	searchBoxHeight := int32(0)
	if e.searchActive {
		searchBoxHeight = 30
	}

	// If no file is open, show welcome screen
	if e.currentFile == "" {
		e.renderWelcomeScreen()
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

	// Render current line highlight (only for saved state when searching, not for search box)
	if e.searchActive && e.savedEditorState != nil {
		cursorY := e.savedEditorState.cursor.y
		if cursorY >= startLine && cursorY < endLine {
			screenY := (cursorY - renderScrollY) * lineHeight
			e.renderer.SetDrawColor(40, 40, 40, 255)
			e.renderer.FillRect(&sdl.Rect{
				X: codeAreaX,
				Y: int32(screenY) + searchBoxHeight,
				W: e.windowWidth - codeAreaX,
				H: lineHeight,
			})
		}
	} else if !e.searchActive && e.cursorY >= startLine && e.cursorY < endLine {
		screenY := (e.cursorY - renderScrollY) * lineHeight
		e.renderer.SetDrawColor(40, 40, 40, 255)
		e.renderer.FillRect(&sdl.Rect{
			X: codeAreaX,
			Y: int32(screenY) + searchBoxHeight,
			W: e.windowWidth - codeAreaX,
			H: lineHeight,
		})
	}

	// Set clip rect for code area to prevent overflow (below search box)
	codeClipRect := &sdl.Rect{
		X: codeAreaX,
		Y: searchBoxHeight,
		W: e.windowWidth - codeAreaX,
		H: e.windowHeight - searchBoxHeight,
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
		rect := &sdl.Rect{X: codeAreaX + int32(gutterWidth-cached.w-10), Y: int32(screenY) + searchBoxHeight, W: cached.w, H: cached.h}
		e.renderer.Copy(cached.texture, nil, rect)
	}

	// Render gutter separator
	e.renderer.SetDrawColor(50, 50, 50, 255)
	e.renderer.DrawLine(codeAreaX+gutterWidth-5, searchBoxHeight, codeAreaX+gutterWidth-5, e.windowHeight)

	// Update clip rect to exclude gutter - only clip text area
	textAreaClipRect := &sdl.Rect{
		X: codeAreaX + gutterWidth,
		Y: searchBoxHeight,
		W: e.windowWidth - codeAreaX - gutterWidth,
		H: e.windowHeight - searchBoxHeight,
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
					Y: int32(screenY) + searchBoxHeight,
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
					Y: int32(screenY) + searchBoxHeight,
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
						Y: int32(screenY) + searchBoxHeight,
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
					Y: int32(screenY) + searchBoxHeight,
					W: int32(endW),
					H: lineHeight,
				})
			}
		}
	}

	// Render search match highlights
	searchQuery := e.buffer.Line(0)
	if e.searchActive && len(e.searchMatches) > 0 && len(searchQuery) > 0 {
		queryLen := len(searchQuery)
		for _, matchIndex := range e.visibleSearchMatchIndexes(startLine, endLine) {
			match := e.searchMatches[matchIndex]
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
					Y: int32(screenY) + searchBoxHeight,
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
		// Expand tabs to spaces for rendering only
		text := expandTabsForDisplay(line, tabWidth)
		cached, err := e.cachedText(text, sdl.Color{R: 200, G: 200, B: 200, A: 255})
		if err != nil {
			continue
		}

		screenY := (y - renderScrollY) * lineHeight

		// Calculate source rect based on horizontal scroll
		srcRect := &sdl.Rect{
			X: int32(renderScrollX),
			Y: 0,
			W: cached.w - int32(renderScrollX),
			H: cached.h,
		}

		// Don't render if scrolled past the entire line
		if srcRect.W <= 0 {
			continue
		}

		dstRect := &sdl.Rect{
			X: codeAreaX + int32(gutterWidth),
			Y: int32(screenY) + searchBoxHeight,
			W: srcRect.W,
			H: cached.h,
		}

		e.renderer.Copy(cached.texture, srcRect, dstRect)
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
		e.renderer.FillRect(&sdl.Rect{X: cursorX, Y: int32(cursorY) + searchBoxHeight, W: 2, H: fontSize})
	}

	// Clear clip rect
	e.renderer.SetClipRect(nil)

	// Render search box on top
	e.renderSearchBox()

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

	// Check if we're running detached already
	isDetached := false
	var targetPath string

	// Parse arguments
	args := os.Args[1:]
	for i, arg := range args {
		if arg == "--detached" {
			isDetached = true
		} else if i == len(args)-1 {
			// Last non-flag argument is the target path
			targetPath = arg
		}
	}

	// If not detached, re-execute ourselves in detached mode
	if !isDetached {
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to get executable path: %v\n", err)
			os.Exit(1)
		}

		// Build arguments for detached process
		detachedArgs := []string{"--detached"}
		if targetPath != "" {
			detachedArgs = append(detachedArgs, targetPath)
		}

		cmd := exec.Command(exe, detachedArgs...)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setsid: true, // Create new session and detach from terminal
		}

		// Start the detached process
		err = cmd.Start()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to start detached process: %v\n", err)
			os.Exit(1)
		}

		// Parent process exits immediately
		return
	}

	// We're the detached child process, run the editor
	editor, err := NewEditor(targetPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create editor: %v\n", err)
		os.Exit(1)
	}
	defer editor.cleanup()

	editor.run()
}
