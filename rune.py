#!/usr/bin/env python3
import json
import os
import re
import sys
import time
import tkinter as tk
from dataclasses import dataclass, field
from pathlib import Path
from tkinter import filedialog, messagebox, simpledialog, ttk


APP = "Rune"
DEFAULT_IGNORE = {".git", ".rune", "node_modules", "dist", "vendor", "__pycache__"}
KEYWORDS = {
    "and", "as", "async", "await", "break", "case", "catch", "class", "const",
    "continue", "def", "default", "defer", "del", "do", "elif", "else", "enum",
    "export", "extends", "false", "finally", "for", "from", "func", "function",
    "go", "if", "import", "in", "interface", "is", "let", "match", "new", "nil",
    "none", "not", "null", "or", "package", "pass", "return", "self", "static",
    "struct", "switch", "this", "throw", "true", "try", "type", "var", "while",
    "with", "yield",
}


def clamp(value, lo, hi):
    return max(lo, min(hi, value))


def is_binary(data):
    if not data:
        return False
    sample = data[:4096]
    if b"\0" in sample:
        return True
    controls = sum(1 for b in sample if b < 9 or (13 < b < 32))
    return controls * 8 > len(sample)


def detect_ending(data):
    return "\r\n" if b"\r\n" in data else "\n"


def normalize_newlines(text):
    return text.replace("\r\n", "\n").replace("\r", "\n")


def apply_ending(text, ending):
    return text.replace("\n", "\r\n") if ending == "\r\n" else text


def safe_name(path):
    return "".join("_" if c in ':\\/?*"<>|' else c for c in str(path))


def fuzzy_score(candidate, query):
    candidate_low = candidate.lower()
    query_low = query.strip().lower()
    if not query_low:
        return 1
    score = 0
    pos = 0
    for ch in query_low:
        found = False
        while pos < len(candidate_low):
            if candidate_low[pos] == ch:
                score += 10
                if pos == 0 or candidate_low[pos - 1] in "\\/-_ .":
                    score += 5
                pos += 1
                found = True
                break
            pos += 1
            score -= 1
        if not found:
            return -10**9
    return score


def fuzzy_list(items, query, limit=80):
    scored = [(fuzzy_score(item, query), item) for item in items]
    scored = [item for item in scored if item[0] > -10**9]
    scored.sort(key=lambda item: (-item[0], item[1].lower()))
    return [item for _, item in scored[:limit]]


@dataclass
class Buffer:
    app: "RuneApp"
    path: Path | None = None
    title: str = "untitled"
    line_ending: str = "\n"
    read_only: bool = False
    mod_time: float = 0.0
    dirty: bool = False
    last_autosave: float = 0.0
    text: tk.Text | None = None
    gutter: tk.Canvas | None = None
    tab_id: str | None = None
    search_matches: list[tuple[str, str]] = field(default_factory=list)
    current_match: int = -1

    def key(self):
        return str(self.path) if self.path else self.title

    def display_name(self):
        name = self.path.name if self.path else self.title
        return name + (" *" if self.dirty else "")

    def content(self):
        return self.text.get("1.0", "end-1c") if self.text else ""

    def autosave_path(self):
        base = self.path if self.path else self.title
        return self.app.autosave_dir / (safe_name(base) + ".autosave")

    def mark_dirty(self, dirty=True):
        self.dirty = dirty
        if self.tab_id:
            self.app.tabs.tab(self.tab_id, text=self.display_name())
        self.app.update_title()
        if dirty:
            self.maybe_autosave()

    def maybe_autosave(self):
        now = time.time()
        if now - self.last_autosave < self.app.autosave_interval:
            return
        try:
            self.autosave_path().write_text(self.content(), encoding="utf-8")
            self.last_autosave = now
        except OSError:
            pass

    def clear_autosave(self):
        try:
            self.autosave_path().unlink()
        except OSError:
            pass


class EntryDialog(tk.Toplevel):
    def __init__(self, app, title, prompt, initial="", provider=None):
        super().__init__(app.root)
        self.app = app
        self.result = None
        self.provider = provider
        self.title(title)
        self.transient(app.root)
        self.configure(bg=app.theme["panel"])
        self.geometry("720x330+160+120")
        self.resizable(True, True)
        self.label = tk.Label(self, text=prompt, anchor="w", bg=app.theme["panel"], fg=app.theme["muted"])
        self.label.pack(fill="x", padx=12, pady=(12, 4))
        self.entry = tk.Entry(self, bg=app.theme["input_bg"], fg=app.theme["foreground"], insertbackground=app.theme["foreground"])
        self.entry.pack(fill="x", padx=12)
        self.entry.insert(0, initial)
        self.listbox = tk.Listbox(self, bg=app.theme["panel"], fg=app.theme["foreground"], selectbackground=app.theme["accent"], activestyle="none")
        self.listbox.pack(fill="both", expand=True, padx=12, pady=12)
        self.entry.bind("<KeyRelease>", lambda _event: self.refresh())
        self.entry.bind("<Return>", lambda _event: self.accept())
        self.entry.bind("<Escape>", lambda _event: self.cancel())
        self.listbox.bind("<Return>", lambda _event: self.accept())
        self.listbox.bind("<Double-Button-1>", lambda _event: self.accept())
        self.bind("<Escape>", lambda _event: self.cancel())
        self.protocol("WM_DELETE_WINDOW", self.cancel)
        self.refresh()
        self.entry.focus_set()
        self.grab_set()

    def refresh(self):
        self.listbox.delete(0, "end")
        if not self.provider:
            return
        for item in self.provider(self.entry.get()):
            self.listbox.insert("end", item)
        if self.listbox.size():
            self.listbox.selection_set(0)

    def accept(self):
        if self.listbox.focus_get() == self.listbox and self.listbox.curselection():
            self.result = self.listbox.get(self.listbox.curselection()[0])
        elif self.listbox.curselection() and self.provider:
            self.result = self.listbox.get(self.listbox.curselection()[0])
        else:
            self.result = self.entry.get()
        self.destroy()

    def cancel(self):
        self.result = None
        self.destroy()


class FindDialog(tk.Toplevel):
    def __init__(self, app):
        super().__init__(app.root)
        self.app = app
        self.title("Find")
        self.transient(app.root)
        self.configure(bg=app.theme["panel"])
        self.geometry("560x170+180+140")
        self.query = tk.StringVar(value=app.last_find)
        self.replace = tk.StringVar(value=app.last_replace)
        self.case = tk.BooleanVar(value=app.find_case)
        self.word = tk.BooleanVar(value=app.find_word)
        self.regex = tk.BooleanVar(value=app.find_regex)
        self._row("Find", self.query, 0)
        self._row("Replace", self.replace, 1)
        opts = tk.Frame(self, bg=app.theme["panel"])
        opts.grid(row=2, column=0, columnspan=4, sticky="ew", padx=10, pady=6)
        for label, var in [("Case", self.case), ("Word", self.word), ("Regex", self.regex)]:
            tk.Checkbutton(opts, text=label, variable=var, bg=app.theme["panel"], fg=app.theme["foreground"], selectcolor=app.theme["input_bg"]).pack(side="left", padx=4)
        for label, cmd in [("Next", self.find_next), ("Prev", self.find_prev), ("Replace", self.replace_one), ("Replace All", self.replace_all)]:
            tk.Button(opts, text=label, command=cmd).pack(side="right", padx=3)
        self.bind("<Escape>", lambda _event: self.destroy())
        self.protocol("WM_DELETE_WINDOW", self.destroy)
        self.grid_columnconfigure(1, weight=1)
        self.entries[0].focus_set()

    def _row(self, label, var, row):
        if not hasattr(self, "entries"):
            self.entries = []
        tk.Label(self, text=label, bg=self.app.theme["panel"], fg=self.app.theme["muted"]).grid(row=row, column=0, sticky="w", padx=10, pady=6)
        entry = tk.Entry(self, textvariable=var, bg=self.app.theme["input_bg"], fg=self.app.theme["foreground"], insertbackground=self.app.theme["foreground"])
        entry.grid(row=row, column=1, columnspan=3, sticky="ew", padx=10, pady=6)
        entry.bind("<Return>", lambda _event: self.find_next())
        self.entries.append(entry)

    def sync(self):
        self.app.last_find = self.query.get()
        self.app.last_replace = self.replace.get()
        self.app.find_case = self.case.get()
        self.app.find_word = self.word.get()
        self.app.find_regex = self.regex.get()

    def find_next(self):
        self.sync()
        self.app.find_next()

    def find_prev(self):
        self.sync()
        self.app.find_prev()

    def replace_one(self):
        self.sync()
        self.app.replace_current()

    def replace_all(self):
        self.sync()
        self.app.replace_all()


class RuneApp:
    def __init__(self, root, root_dir):
        self.root = root
        self.root_dir = Path(root_dir).resolve()
        self.state_dir = self.root_dir / ".rune"
        self.autosave_dir = self.state_dir / "autosave"
        self.state_dir.mkdir(exist_ok=True)
        self.autosave_dir.mkdir(exist_ok=True)
        self.config = self.load_config()
        self.ignore = set(DEFAULT_IGNORE) | set(self.config.get("ignore", []))
        self.autosave_interval = max(1, int(self.config.get("autosave_ms", 3000)) / 1000.0)
        self.theme = self.default_theme()
        self.theme.update(self.config.get("theme", {}))
        self.keybindings = self.config.get("keybindings", {})
        self.buffers: list[Buffer] = []
        self.recent_files: list[str] = []
        self.last_find = ""
        self.last_replace = ""
        self.find_case = False
        self.find_word = False
        self.find_regex = False
        self.show_whitespace = bool(self.config.get("show_whitespace", False))
        self.wrap = bool(self.config.get("word_wrap", False))
        self.reload_on_changed = bool(self.config.get("reload_on_changed", True))
        self.match_tag = "matching_bracket"
        self._project_files_cache = []
        self._project_files_mtime = 0.0
        self.root.title(APP)
        self.root.geometry(self.config.get("geometry", "1100x750"))
        self.root.protocol("WM_DELETE_WINDOW", self.quit)
        self.build_ui()
        self.bind_keys()
        self.refresh_tree()
        self.restore_session()
        if not self.buffers:
            self.new_buffer()
        self.root.after(1500, self.periodic)

    def default_theme(self):
        return {
            "background": "#0f0f0f",
            "panel": "#171717",
            "input_bg": "#202020",
            "foreground": "#d6d6d6",
            "muted": "#888888",
            "accent": "#5fa8d3",
            "selection": "#315070",
            "comment": "#7a9b72",
            "string": "#d7a96d",
            "number": "#be95dc",
            "keyword": "#75b6f0",
            "bracket": "#b97be8",
            "danger": "#c45c5c",
        }

    def load_config(self):
        for path in (self.state_dir / "config.json", self.root_dir / ".rune.json"):
            try:
                return json.loads(path.read_text(encoding="utf-8"))
            except OSError:
                pass
            except json.JSONDecodeError:
                pass
        return {}

    def build_ui(self):
        self.root.configure(bg=self.theme["background"])
        self.topbar = tk.Frame(self.root, bg=self.theme["panel"], height=28)
        self.topbar.pack(fill="x")
        for label, command in [
            ("Command", self.command_palette),
            ("File", self.fuzzy_file),
            ("Buffer", self.buffer_switcher),
            ("Find", self.find_dialog),
            ("Grep", self.project_grep),
        ]:
            tk.Button(
                self.topbar,
                text=label,
                command=command,
                bg=self.theme["panel"],
                fg=self.theme["foreground"],
                activebackground="#2d3742",
                activeforeground=self.theme["foreground"],
                relief="flat",
                padx=10,
            ).pack(side="left")
        self.path_label = tk.Label(self.topbar, text="", bg=self.theme["panel"], fg=self.theme["muted"], anchor="e")
        self.path_label.pack(side="right", fill="x", expand=True, padx=10)
        self.pane = ttk.PanedWindow(self.root, orient="horizontal")
        self.pane.pack(fill="both", expand=True)
        self.left = tk.Frame(self.pane, bg=self.theme["panel"])
        self.tree = ttk.Treeview(self.left, show="tree")
        self.tree.pack(fill="both", expand=True)
        self.tree.bind("<Double-1>", self.tree_open)
        self.tree.bind("<Return>", self.tree_open)
        self.tree.bind("<<TreeviewOpen>>", self.tree_expand)
        self.pane.add(self.left, weight=0)
        self.tabs = ttk.Notebook(self.pane)
        self.tabs.bind("<<NotebookTabChanged>>", lambda _event: self.update_title())
        self.pane.add(self.tabs, weight=1)
        self.style = ttk.Style()
        try:
            self.style.theme_use("clam")
        except tk.TclError:
            pass
        self.style.configure("Treeview", background=self.theme["panel"], foreground=self.theme["foreground"], fieldbackground=self.theme["panel"], borderwidth=0, rowheight=22)
        self.style.map("Treeview", background=[("selected", self.theme["selection"])])
        self.style.configure("TNotebook", background=self.theme["background"], borderwidth=0)
        self.style.configure("TNotebook.Tab", background=self.theme["panel"], foreground=self.theme["foreground"], padding=(10, 5))
        self.style.map("TNotebook.Tab", background=[("selected", "#2d3742")])
        self.root.update_idletasks()
        self.pane.sashpos(0, int(self.config.get("browser_width", 260)))

    def bind_keys(self):
        binds = {
            "<Control-Shift-P>": self.command_palette,
            "<Control-p>": self.fuzzy_file,
            "<Control-b>": self.buffer_switcher,
            "<Control-o>": self.open_path_dialog,
            "<Control-n>": self.new_buffer,
            "<Control-Shift-N>": self.create_file,
            "<Control-s>": self.save_current,
            "<Control-Shift-S>": self.save_as,
            "<Control-g>": self.go_to_line,
            "<Control-Shift-F>": self.project_grep,
            "<Control-w>": self.close_current,
            "<Control-d>": self.duplicate_line,
            "<Control-Shift-K>": self.delete_line,
            "<Alt-Up>": lambda: self.move_line(-1),
            "<Alt-Down>": lambda: self.move_line(1),
            "<Control-bracketright>": self.indent,
            "<Control-bracketleft>": self.unindent,
            "<Alt-grave>": self.toggle_whitespace,
            "<Alt-z>": self.toggle_wrap,
            "<Control-f>": self.find_dialog,
            "<F3>": self.find_next,
            "<Shift-F3>": self.find_prev,
            "<Control-l>": self.reload_current,
        }
        for sequence, command in binds.items():
            self.root.bind_all(sequence, self.wrap_event(command))
        for sequence, command in self.keybindings.items():
            self.root.bind_all(sequence, self.wrap_event(lambda c=command: self.run_command(c)))

    def wrap_event(self, func):
        def inner(event=None):
            func()
            return "break"
        return inner

    def current(self):
        selected = self.tabs.select()
        for buf in self.buffers:
            if str(buf.tab_id) == str(selected):
                return buf
        return self.buffers[-1] if self.buffers else None

    def make_text(self, buf):
        frame = tk.Frame(self.tabs, bg=self.theme["background"])
        gutter = tk.Canvas(frame, width=58, highlightthickness=0, bg=self.theme["background"])
        text = tk.Text(
            frame,
            undo=True,
            maxundo=1000,
            wrap="word" if self.wrap else "none",
            bg=self.theme["background"],
            fg=self.theme["foreground"],
            insertbackground=self.theme["foreground"],
            selectbackground=self.theme["selection"],
            relief="flat",
            borderwidth=0,
            padx=8,
            pady=8,
            font=("JetBrains Mono", 11),
        )
        def yview(*args):
            text.yview(*args)
            self.render_gutter(buf)
            self.highlight_visible(buf)
        def yscroll_changed(first, last):
            yscroll.set(first, last)
            self.render_gutter(buf)
        yscroll = ttk.Scrollbar(frame, orient="vertical", command=yview)
        xscroll = ttk.Scrollbar(frame, orient="horizontal", command=text.xview)
        text.configure(yscrollcommand=yscroll_changed, xscrollcommand=xscroll.set)
        gutter.pack(side="left", fill="y")
        yscroll.pack(side="right", fill="y")
        xscroll.pack(side="bottom", fill="x")
        text.pack(fill="both", expand=True)
        text.bind("<<Modified>>", lambda event, b=buf: self.modified(b))
        text.bind("<Return>", lambda event, b=buf: self.auto_indent(b))
        text.bind("<Control-c>", lambda event: self.copy())
        text.bind("<Control-x>", lambda event: self.cut())
        text.bind("<KeyRelease>", lambda event, b=buf: self.after_key(b))
        text.bind("<Configure>", lambda event, b=buf: (self.render_gutter(b), self.highlight_visible(b)))
        text.bind("<MouseWheel>", lambda event, b=buf: self.root.after_idle(lambda: (self.render_gutter(b), self.highlight_visible(b))))
        buf.text = text
        buf.gutter = gutter
        buf.tab_id = frame
        self.tabs.add(frame, text=buf.display_name())
        self.tabs.select(frame)
        self.configure_tags(buf)
        return text

    def configure_tags(self, buf):
        t = buf.text
        t.tag_configure("keyword", foreground=self.theme["keyword"])
        t.tag_configure("comment", foreground=self.theme["comment"])
        t.tag_configure("string", foreground=self.theme["string"])
        t.tag_configure("number", foreground=self.theme["number"])
        t.tag_configure("whitespace", background="#2a2a2a")
        t.tag_configure(self.match_tag, background=self.theme["bracket"], foreground="#ffffff")
        t.tag_configure("grep_hit", background="#4a3a20")
        t.tag_configure("current_line", background="#282828")

    def modified(self, buf):
        if not buf.text.edit_modified():
            return
        buf.text.edit_modified(False)
        buf.mark_dirty(True)
        self.highlight_later(buf)

    def after_key(self, buf):
        self.highlight_later(buf)
        self.match_bracket(buf)
        self.render_gutter(buf)
        self.highlight_current_line(buf)

    def highlight_later(self, buf):
        if getattr(buf, "_highlight_after", None):
            self.root.after_cancel(buf._highlight_after)
        buf._highlight_after = self.root.after(80, lambda: self.highlight_visible(buf))

    def highlight(self, buf):
        self.highlight_visible(buf, full=True)

    def highlight_visible(self, buf, full=False):
        if not buf.text:
            return
        text = buf.text
        if full:
            start_line = 1
            end_line = int(text.index("end-1c").split(".")[0])
        else:
            try:
                start_line = int(text.index("@0,0").split(".")[0])
                end_line = int(text.index(f"@0,{text.winfo_height()}").split(".")[0]) + 2
            except tk.TclError:
                return
        start = f"{start_line}.0"
        end = f"{end_line}.end"
        for tag in ("keyword", "comment", "string", "number", "whitespace"):
            text.tag_remove(tag, start, end)
        for line_no in range(start_line, end_line + 1):
            line = text.get(f"{line_no}.0", f"{line_no}.end")
            for match in re.finditer(r"//.*|#.*", line):
                self.tag_line_span(text, "comment", line_no, match.start(), match.end())
            for match in re.finditer(r'"(?:\\.|[^"\\])*"|\'(?:\\.|[^\'\\])*\'', line):
                self.tag_line_span(text, "string", line_no, match.start(), match.end())
            for match in re.finditer(r"\b\d+(?:\.\d+)?\b", line):
                self.tag_line_span(text, "number", line_no, match.start(), match.end())
            for match in re.finditer(r"\b[A-Za-z_][A-Za-z0-9_]*\b", line):
                if match.group(0).lower() in KEYWORDS:
                    self.tag_line_span(text, "keyword", line_no, match.start(), match.end())
            if self.show_whitespace:
                for match in re.finditer(r"[ \t]+", line):
                    self.tag_line_span(text, "whitespace", line_no, match.start(), match.end())
        self.highlight_current_line(buf)

    def highlight_current_line(self, buf):
        text = buf.text
        text.tag_remove("current_line", "1.0", "end")
        line = text.index("insert").split(".")[0]
        text.tag_add("current_line", f"{line}.0", f"{line}.end+1c")
        text.tag_lower("current_line")

    def render_gutter(self, buf):
        if not buf.text or not buf.gutter:
            return
        text, gutter = buf.text, buf.gutter
        gutter.delete("all")
        current = int(text.index("insert").split(".")[0])
        try:
            start_line = int(text.index("@0,0").split(".")[0])
            end_line = int(text.index(f"@0,{text.winfo_height()}").split(".")[0]) + 1
        except tk.TclError:
            return
        for line_no in range(start_line, end_line + 1):
            bbox = text.dlineinfo(f"{line_no}.0")
            if not bbox:
                continue
            y = bbox[1]
            color = self.theme["accent"] if line_no == current else self.theme["muted"]
            gutter.create_text(50, y, anchor="ne", text=str(line_no), fill=color, font=("JetBrains Mono", 10))

    def tag_line_span(self, text, tag, line, start, end):
        text.tag_add(tag, f"{line}.{start}", f"{line}.{end}")

    def tag_span(self, text, tag, start, end):
        text.tag_add(tag, f"1.0+{start}c", f"1.0+{end}c")

    def new_buffer(self):
        title = f"untitled-{sum(1 for b in self.buffers if not b.path) + 1}"
        buf = Buffer(self, title=title, dirty=True)
        self.buffers.append(buf)
        text = self.make_text(buf)
        text.edit_modified(False)
        buf.mark_dirty(True)
        self.update_title()

    def open_buffer(self, path):
        path = Path(path).resolve()
        for buf in self.buffers:
            if buf.path == path:
                self.tabs.select(buf.tab_id)
                return buf
        try:
            data = path.read_bytes()
        except OSError as exc:
            messagebox.showerror(APP, str(exc))
            return None
        if is_binary(data):
            messagebox.showwarning(APP, "Refusing to open likely binary file.")
            return None
        ending = detect_ending(data)
        text = normalize_newlines(data.decode("utf-8", errors="replace"))
        auto = self.autosave_dir / (safe_name(path) + ".autosave")
        dirty = False
        try:
            if auto.exists() and auto.stat().st_mtime > path.stat().st_mtime:
                text = auto.read_text(encoding="utf-8")
                dirty = True
        except OSError:
            pass
        mode = path.stat().st_mode
        buf = Buffer(self, path=path, title=path.name, line_ending=ending, read_only=not bool(mode & 0o200), mod_time=path.stat().st_mtime, dirty=dirty)
        self.buffers.append(buf)
        widget = self.make_text(buf)
        widget.insert("1.0", text)
        widget.edit_modified(False)
        buf.mark_dirty(dirty)
        self.add_recent(path)
        self.highlight(buf)
        self.save_session()
        return buf

    def open_path_dialog(self):
        path = filedialog.askopenfilename(initialdir=self.root_dir)
        if path:
            self.open_buffer(path)

    def create_file(self):
        path = filedialog.asksaveasfilename(initialdir=self.root_dir)
        if not path:
            return
        path = Path(path)
        if path.exists():
            messagebox.showwarning(APP, "File already exists.")
            return
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("", encoding="utf-8")
        self.refresh_tree()
        self.open_buffer(path)

    def save_current(self):
        buf = self.current()
        if not buf:
            return
        if not buf.path:
            self.save_as()
            return
        if buf.read_only:
            messagebox.showerror(APP, "File is read-only.")
            return
        try:
            buf.path.write_text(apply_ending(buf.content(), buf.line_ending), encoding="utf-8", newline="")
            buf.mod_time = buf.path.stat().st_mtime
            buf.mark_dirty(False)
            buf.clear_autosave()
            self.save_session()
        except OSError as exc:
            messagebox.showerror(APP, str(exc))

    def save_as(self):
        buf = self.current()
        if not buf:
            return
        path = filedialog.asksaveasfilename(initialdir=self.root_dir, initialfile=buf.display_name().rstrip(" *"))
        if not path:
            return
        buf.path = Path(path).resolve()
        buf.title = buf.path.name
        buf.read_only = False
        self.save_current()
        self.refresh_tree()
        self.add_recent(buf.path)

    def close_current(self):
        buf = self.current()
        if not buf:
            return
        if buf.dirty and not self.confirm_dirty(buf):
            return
        self.tabs.forget(buf.tab_id)
        self.buffers.remove(buf)
        self.save_session()
        if not self.buffers:
            self.new_buffer()

    def confirm_dirty(self, buf):
        result = messagebox.askyesnocancel(APP, f"Save changes to {buf.display_name().rstrip(' *')}?")
        if result is None:
            return False
        if result:
            self.save_current()
            return not buf.dirty
        return True

    def quit(self):
        for buf in list(self.buffers):
            self.tabs.select(buf.tab_id)
            if buf.dirty and not self.confirm_dirty(buf):
                return
        self.save_session()
        self.root.destroy()

    def copy(self):
        buf = self.current()
        if not buf:
            return "break"
        text = buf.text
        try:
            data = text.get("sel.first", "sel.last")
        except tk.TclError:
            line = text.index("insert").split(".")[0]
            data = text.get(f"{line}.0", f"{line}.end") + "\n"
        self.root.clipboard_clear()
        self.root.clipboard_append(data)
        return "break"

    def cut(self):
        buf = self.current()
        if not buf:
            return "break"
        text = buf.text
        try:
            data = text.get("sel.first", "sel.last")
            text.delete("sel.first", "sel.last")
        except tk.TclError:
            line = text.index("insert").split(".")[0]
            data = text.get(f"{line}.0", f"{line}.end") + "\n"
            text.delete(f"{line}.0", f"{line}.0 lineend+1c")
        self.root.clipboard_clear()
        self.root.clipboard_append(data)
        buf.mark_dirty(True)
        return "break"

    def auto_indent(self, buf):
        text = buf.text
        line_no, col = map(int, text.index("insert").split("."))
        line = text.get(f"{line_no}.0", f"{line_no}.end")
        indent = re.match(r"[ \t]*", line).group(0)
        before = line[:col].strip()
        if before.endswith(("{", ":", "(", "[")):
            indent += "\t"
        text.insert("insert", "\n" + indent)
        buf.mark_dirty(True)
        return "break"

    def line_range(self):
        text = self.current().text
        try:
            start = text.index("sel.first")
            end = text.index("sel.last")
        except tk.TclError:
            start = end = text.index("insert")
        start_line = int(start.split(".")[0])
        end_line = int(end.split(".")[0])
        return start_line, end_line

    def duplicate_line(self):
        buf = self.current()
        if not buf:
            return
        text = buf.text
        line = text.index("insert").split(".")[0]
        data = text.get(f"{line}.0", f"{line}.end")
        text.insert(f"{line}.end", "\n" + data)
        buf.mark_dirty(True)

    def delete_line(self):
        buf = self.current()
        if not buf:
            return
        text = buf.text
        start, end = self.line_range()
        text.delete(f"{start}.0", f"{end}.0 lineend+1c")
        buf.mark_dirty(True)

    def move_line(self, delta):
        buf = self.current()
        if not buf:
            return
        text = buf.text
        line = int(text.index("insert").split(".")[0])
        other = line + delta
        if other < 1 or other > int(text.index("end-1c").split(".")[0]):
            return
        a = text.get(f"{line}.0", f"{line}.end")
        b = text.get(f"{other}.0", f"{other}.end")
        text.delete(f"{line}.0", f"{line}.end")
        text.insert(f"{line}.0", b)
        text.delete(f"{other}.0", f"{other}.end")
        text.insert(f"{other}.0", a)
        text.mark_set("insert", f"{other}.0")
        buf.mark_dirty(True)

    def indent(self):
        self.shift_indent("\t")

    def unindent(self):
        self.shift_indent("", remove=True)

    def shift_indent(self, prefix, remove=False):
        buf = self.current()
        if not buf:
            return
        text = buf.text
        start, end = self.line_range()
        for line in range(start, end + 1):
            if remove:
                data = text.get(f"{line}.0", f"{line}.0+1c")
                if data == "\t":
                    text.delete(f"{line}.0", f"{line}.0+1c")
                elif text.get(f"{line}.0", f"{line}.0+4c") == "    ":
                    text.delete(f"{line}.0", f"{line}.0+4c")
            else:
                text.insert(f"{line}.0", prefix)
        buf.mark_dirty(True)

    def go_to_line(self):
        buf = self.current()
        if not buf:
            return
        value = simpledialog.askinteger(APP, "Go to line:", parent=self.root, minvalue=1)
        if value:
            buf.text.mark_set("insert", f"{value}.0")
            buf.text.see("insert")

    def find_dialog(self):
        FindDialog(self)

    def build_pattern(self):
        query = self.last_find
        if not query:
            return None
        flags = 0 if self.find_case else re.IGNORECASE
        pattern = query if self.find_regex else re.escape(query)
        if self.find_word:
            pattern = r"\b" + pattern + r"\b"
        try:
            return re.compile(pattern, flags)
        except re.error as exc:
            messagebox.showerror(APP, str(exc))
            return None

    def find_all(self):
        buf = self.current()
        pattern = self.build_pattern()
        if not buf or not pattern:
            return []
        content = buf.content()
        return [(m.start(), m.end()) for m in pattern.finditer(content)]

    def find_next(self):
        self.find_move(1)

    def find_prev(self):
        self.find_move(-1)

    def find_move(self, direction):
        buf = self.current()
        if not buf:
            return
        matches = self.find_all()
        if not matches:
            return
        current = int(buf.text.count("1.0", "insert", "chars")[0])
        if direction > 0:
            match = next((m for m in matches if m[0] > current), matches[0])
        else:
            previous = [m for m in matches if m[0] < current]
            match = previous[-1] if previous else matches[-1]
        self.select_span(buf.text, match[0], match[1])

    def replace_current(self):
        buf = self.current()
        if not buf:
            return
        try:
            buf.text.delete("sel.first", "sel.last")
            buf.text.insert("insert", self.last_replace)
            buf.mark_dirty(True)
        except tk.TclError:
            self.find_next()

    def replace_all(self):
        buf = self.current()
        pattern = self.build_pattern()
        if not buf or not pattern:
            return
        content = buf.content()
        new_content, count = pattern.subn(self.last_replace, content)
        if count:
            buf.text.delete("1.0", "end")
            buf.text.insert("1.0", new_content)
            buf.mark_dirty(True)
            self.highlight(buf)

    def select_span(self, text, start, end):
        a = f"1.0+{start}c"
        b = f"1.0+{end}c"
        text.tag_remove("sel", "1.0", "end")
        text.tag_add("sel", a, b)
        text.mark_set("insert", b)
        text.see(a)

    def match_bracket(self, buf):
        text = buf.text
        text.tag_remove(self.match_tag, "1.0", "end")
        content = buf.content()
        pos = int(text.count("1.0", "insert", "chars")[0])
        candidates = [pos, pos - 1]
        pairs = {"(": (")", 1), "[": ("]", 1), "{": ("}", 1), ")": ("(", -1), "]": ("[", -1), "}": ("{", -1)}
        for at in candidates:
            if at < 0 or at >= len(content) or content[at] not in pairs:
                continue
            target, step = pairs[content[at]]
            depth = 0
            i = at
            while 0 <= i < len(content):
                if content[i] == content[at]:
                    depth += 1
                elif content[i] == target:
                    depth -= 1
                    if depth == 0:
                        self.tag_span(text, self.match_tag, i, i + 1)
                        return
                i += step

    def toggle_whitespace(self):
        self.show_whitespace = not self.show_whitespace
        for buf in self.buffers:
            self.highlight(buf)

    def toggle_wrap(self):
        self.wrap = not self.wrap
        for buf in self.buffers:
            buf.text.configure(wrap="word" if self.wrap else "none")

    def all_project_files(self):
        if self._project_files_cache and time.time() - self._project_files_mtime < 5:
            return list(self._project_files_cache)
        files = []
        for base, dirs, names in os.walk(self.root_dir):
            dirs[:] = [d for d in dirs if d not in self.ignore and not d.startswith(".")]
            for name in names:
                if name in self.ignore or name.startswith("."):
                    continue
                path = Path(base) / name
                try:
                    files.append(str(path.relative_to(self.root_dir)))
                except ValueError:
                    files.append(str(path))
        files.sort()
        self._project_files_cache = files
        self._project_files_mtime = time.time()
        return list(files)

    def fuzzy_file(self):
        items = self.all_project_files()
        result = self.ask("Find file", "File:", provider=lambda q: fuzzy_list(items, q, 100))
        if result:
            self.open_buffer(self.root_dir / result)

    def buffer_switcher(self):
        items = [str(b.path) for b in self.buffers if b.path] + self.recent_files
        result = self.ask("Switch buffer", "Buffer:", provider=lambda q: fuzzy_list(items, q, 100))
        if result:
            self.open_buffer(result)

    def project_grep(self):
        def provider(query):
            query = query.strip().lower()
            if not query:
                return []
            results = []
            for rel in self.all_project_files():
                path = self.root_dir / rel
                try:
                    data = path.read_bytes()
                except OSError:
                    continue
                if is_binary(data):
                    continue
                for i, line in enumerate(normalize_newlines(data.decode("utf-8", errors="replace")).split("\n"), 1):
                    if query in line.lower():
                        results.append(f"{rel}:{i}:{line.strip()}")
                        break
                if len(results) >= 100:
                    break
            return results
        result = self.ask("Project grep", "Search:", provider=provider)
        if not result:
            return
        parts = result.split(":", 2)
        if len(parts) >= 2:
            buf = self.open_buffer(self.root_dir / parts[0])
            if buf:
                buf.text.mark_set("insert", f"{parts[1]}.0")
                buf.text.see("insert")
                buf.text.tag_add("grep_hit", f"{parts[1]}.0", f"{parts[1]}.end")

    def command_palette(self):
        commands = [
            "new", "open", "find file", "switch buffer", "recent", "grep", "create",
            "rename", "delete", "goto", "find", "replace", "replace all", "refresh",
            "reload", "save", "save as", "close tab", "duplicate line", "delete line",
            "toggle whitespace", "toggle wrap",
        ]
        result = self.ask("Command", "Command:", provider=lambda q: fuzzy_list(commands, q, 100))
        if result:
            self.run_command(result)

    def run_command(self, command):
        command = command.lower().strip()
        if command in ("new", "new file"):
            self.new_buffer()
        elif command == "open":
            self.open_path_dialog()
        elif command in ("find file", "file"):
            self.fuzzy_file()
        elif command in ("switch buffer", "buffer"):
            self.buffer_switcher()
        elif command == "recent":
            result = self.ask("Recent files", "Recent:", provider=lambda q: fuzzy_list(self.recent_files, q, 100))
            if result:
                self.open_buffer(result)
        elif command in ("grep", "project grep"):
            self.project_grep()
        elif command == "create":
            self.create_file()
        elif command == "rename":
            self.rename_current()
        elif command == "delete":
            self.delete_current_file()
        elif command in ("goto", "go to line"):
            self.go_to_line()
        elif command == "find":
            self.find_dialog()
        elif command == "replace":
            self.find_dialog()
        elif command == "replace all":
            self.replace_all()
        elif command == "refresh":
            self.refresh_tree()
        elif command == "reload":
            self.reload_current()
        elif command == "save":
            self.save_current()
        elif command == "save as":
            self.save_as()
        elif command == "close tab":
            self.close_current()
        elif command == "duplicate line":
            self.duplicate_line()
        elif command == "delete line":
            self.delete_line()
        elif command == "toggle whitespace":
            self.toggle_whitespace()
        elif command == "toggle wrap":
            self.toggle_wrap()
        elif command in ("oracle", "temple"):
            self.theme["background"] = "#0000aa"
            self.theme["accent"] = "#55aaff"
            self.root.configure(bg=self.theme["background"])
            messagebox.showinfo(APP, "640K ought to be enough for God's compiler.")

    def ask(self, title, prompt, initial="", provider=None):
        dialog = EntryDialog(self, title, prompt, initial, provider)
        self.root.wait_window(dialog)
        return dialog.result

    def rename_current(self):
        buf = self.current()
        if not buf or not buf.path:
            return
        new_path = filedialog.asksaveasfilename(initialdir=buf.path.parent, initialfile=buf.path.name)
        if not new_path:
            return
        try:
            Path(new_path).parent.mkdir(parents=True, exist_ok=True)
            buf.path.rename(new_path)
            buf.path = Path(new_path).resolve()
            buf.title = buf.path.name
            self.refresh_tree()
            self.save_session()
        except OSError as exc:
            messagebox.showerror(APP, str(exc))

    def delete_current_file(self):
        buf = self.current()
        if not buf or not buf.path:
            return
        if not messagebox.askyesno(APP, f"Delete {buf.path.name}?"):
            return
        try:
            buf.path.unlink()
            self.close_current()
            self.refresh_tree()
        except OSError as exc:
            messagebox.showerror(APP, str(exc))

    def reload_current(self):
        buf = self.current()
        if not buf or not buf.path:
            return
        if buf.dirty and not messagebox.askyesno(APP, "Discard changes and reload from disk?"):
            return
        path = buf.path
        index = self.buffers.index(buf)
        self.tabs.forget(buf.tab_id)
        self.buffers.remove(buf)
        new_buf = self.open_buffer(path)
        if new_buf and index < len(self.buffers):
            self.tabs.select(new_buf.tab_id)

    def refresh_tree(self):
        self._project_files_cache = []
        self.tree.delete(*self.tree.get_children())
        root_id = self.tree.insert("", "end", text=self.root_dir.name, values=(str(self.root_dir),), open=True)
        self.populate_tree(root_id, self.root_dir)

    def populate_tree(self, parent, path):
        for child in self.tree.get_children(parent):
            self.tree.delete(child)
        try:
            entries = sorted(path.iterdir(), key=lambda p: (not p.is_dir(), p.name.lower()))
        except OSError:
            return
        for entry in entries:
            if entry.name in self.ignore or entry.name.startswith("."):
                continue
            node = self.tree.insert(parent, "end", text=entry.name, values=(str(entry),), open=False)
            if entry.is_dir():
                self.tree.insert(node, "end", text="", values=("",))

    def tree_expand(self, event=None):
        item = self.tree.focus()
        values = self.tree.item(item, "values")
        if values and values[0]:
            path = Path(values[0])
            if path.is_dir():
                self.populate_tree(item, path)

    def tree_open(self, event=None):
        item = self.tree.focus()
        if not item:
            return
        values = self.tree.item(item, "values")
        if not values:
            return
        path = Path(values[0])
        if path.is_file():
            self.open_buffer(path)

    def add_recent(self, path):
        path = str(Path(path).resolve())
        self.recent_files = [path] + [p for p in self.recent_files if p != path]
        self.recent_files = self.recent_files[:30]

    def save_session(self):
        data = {
            "root": str(self.root_dir),
            "open_files": [str(b.path) for b in self.buffers if b.path],
            "current_file": str(self.current().path) if self.current() and self.current().path else "",
            "recent_files": self.recent_files,
            "geometry": self.root.geometry(),
        }
        try:
            (self.state_dir / "session.json").write_text(json.dumps(data, indent=2), encoding="utf-8")
        except OSError:
            pass

    def restore_session(self):
        try:
            data = json.loads((self.state_dir / "session.json").read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            return
        if data.get("root") != str(self.root_dir):
            return
        self.recent_files = data.get("recent_files", [])
        if data.get("geometry"):
            self.root.geometry(data["geometry"])
        for path in data.get("open_files", []):
            if Path(path).exists():
                self.open_buffer(path)
        current = data.get("current_file")
        if current:
            for buf in self.buffers:
                if str(buf.path) == current:
                    self.tabs.select(buf.tab_id)
                    break

    def periodic(self):
        for buf in list(self.buffers):
            if buf.dirty:
                buf.maybe_autosave()
            if self.reload_on_changed and buf.path:
                try:
                    mtime = buf.path.stat().st_mtime
                except OSError:
                    continue
                if buf.mod_time and mtime > buf.mod_time and not buf.dirty:
                    self.tabs.select(buf.tab_id)
                    if messagebox.askyesno(APP, f"{buf.path.name} changed on disk. Reload?"):
                        self.reload_current()
                    else:
                        buf.mod_time = mtime
        self.root.after(1500, self.periodic)

    def update_title(self):
        buf = self.current()
        if not buf:
            self.root.title(APP)
            self.path_label.configure(text="")
            return
        self.root.title(f"{buf.display_name()} | {APP}")
        self.path_label.configure(text=str(buf.path) if buf.path else buf.title)


def main():
    root_dir = Path(sys.argv[1]).resolve() if len(sys.argv) > 1 else Path.cwd()
    root = tk.Tk()
    try:
        root.tk.call("tk", "scaling", 1.1)
    except tk.TclError:
        pass
    RuneApp(root, root_dir)
    root.mainloop()


if __name__ == "__main__":
    main()
