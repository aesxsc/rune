param(
    [string]$OutputDir = "dist/windows-amd64",
    [string]$MsysRoot = "C:/msys64",
    [string]$GoToolchain = "local"
)

$ErrorActionPreference = "Stop"

$mingwBin = Join-Path $MsysRoot "mingw64/bin"
$mingwRoot = Join-Path $MsysRoot "mingw64"
$usrBin = Join-Path $MsysRoot "usr/bin"
$bash = Join-Path $usrBin "bash.exe"

if (-not (Test-Path $mingwBin)) {
    throw "MSYS2 MinGW64 bin directory not found: $mingwBin"
}
if (-not (Test-Path $bash)) {
    throw "MSYS2 bash not found: $bash"
}

$env:Path = "$mingwBin;$usrBin;$env:Path"
$env:GOTOOLCHAIN = $GoToolchain

foreach ($tool in @("gcc", "pkg-config", "go")) {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) {
        throw "Required tool not found on PATH: $tool"
    }
}

pkg-config --exists sdl2 SDL2_ttf
if ($LASTEXITCODE -ne 0) {
    throw "MSYS2 packages sdl2 and SDL2_ttf must be installed for mingw64"
}

New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null
$exePath = Join-Path $OutputDir "rune.exe"

go build -trimpath -ldflags "-s -w" -o $exePath .
if ($LASTEXITCODE -ne 0) {
    throw "go build failed"
}

$repo = (Get-Location).Path
$bashRepo = $repo -replace "\\", "/"
$bashExe = "$bashRepo/$($exePath -replace "\\", "/")"

$lddOutput = & $bash -lc "export PATH=/mingw64/bin:/usr/bin:`$PATH; ldd '$bashExe'"
if ($LASTEXITCODE -ne 0) {
    throw "ldd failed while scanning runtime DLL dependencies"
}

$copied = @{}
foreach ($line in $lddOutput) {
    if ($line -match "(?:=>\s+)?(/mingw64/bin/[^ ]+\.dll)") {
        $bashPath = $Matches[1]
        $winPath = $bashPath -replace "^/mingw64", ($mingwRoot -replace "\\", "/")
        $winPath = $winPath -replace "/", "\"
        $name = Split-Path $winPath -Leaf
        if (-not $copied.ContainsKey($name)) {
            Copy-Item -Force -Path $winPath -Destination (Join-Path $OutputDir $name)
            $copied[$name] = $true
        }
    }
}

Write-Host "Built $exePath"
Write-Host "Copied $($copied.Count) MinGW runtime DLLs"
