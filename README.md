# Rune
An extremely simple and lightweight code editor written in Go.

## Build

Rune uses SDL2 through cgo. On Windows, install the MSYS2 MinGW64 packages first:

```powershell
pacman -S --needed mingw-w64-x86_64-gcc mingw-w64-x86_64-pkgconf mingw-w64-x86_64-SDL2 mingw-w64-x86_64-SDL2_ttf
```

Build and run from the repository root:

```powershell
go build .
.\rune.exe .
```

The program source lives in `rune.go`.
