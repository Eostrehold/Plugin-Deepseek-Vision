$ErrorActionPreference = "Stop"

$version = "20260616"
$archiveName = "llvm-mingw-$version-ucrt-aarch64.zip"
$expectedSha256 = "312593669435bd0bfc1a43ac3fba23c8b27e0610bade88b2738e5a01702a99ba"
$archivePath = Join-Path $env:RUNNER_TEMP $archiveName
$toolchainRoot = Join-Path $env:RUNNER_TEMP "llvm-mingw-$version-ucrt-aarch64"
$downloadUrl = "https://github.com/mstorsjo/llvm-mingw/releases/download/$version/$archiveName"

Invoke-WebRequest -Uri $downloadUrl -OutFile $archivePath
$actualSha256 = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
if ($actualSha256 -ne $expectedSha256) {
    throw "llvm-mingw checksum mismatch: expected $expectedSha256, got $actualSha256"
}

Expand-Archive -Path $archivePath -DestinationPath $env:RUNNER_TEMP -Force
$binDir = Join-Path $toolchainRoot "bin"
$compiler = Join-Path $binDir "aarch64-w64-mingw32-gcc.exe"
if (-not (Test-Path -Path $compiler -PathType Leaf)) {
    throw "Windows ARM64 compiler not found after extraction: $compiler"
}

Add-Content -Path $env:GITHUB_PATH -Value $binDir -Encoding utf8
Add-Content -Path $env:GITHUB_ENV -Value "CC=aarch64-w64-mingw32-gcc" -Encoding utf8
Add-Content -Path $env:GITHUB_ENV -Value "CXX=aarch64-w64-mingw32-g++" -Encoding utf8
Add-Content -Path $env:GITHUB_ENV -Value "OBJDUMP=aarch64-w64-mingw32-objdump" -Encoding utf8

& $compiler --version
