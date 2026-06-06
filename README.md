# Rune
An extremely simple and lightweight code editor written in Go.

## Windows build

Rune uses SDL2 through cgo. On Windows, install the MSYS2 MinGW64 packages first:

```powershell
pacman -S --needed mingw-w64-x86_64-gcc mingw-w64-x86_64-pkgconf mingw-w64-x86_64-SDL2 mingw-w64-x86_64-SDL2_ttf
```

Then build a runnable Windows folder:

```powershell
.\scripts\build-windows.ps1
```

The output is written to `dist/windows-amd64/rune.exe` with the required MinGW SDL runtime DLLs copied beside it.
