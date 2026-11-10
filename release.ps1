# Baut ein Release nach .\dist\release:
#   BastionServer-Setup-<Version>.exe       Windows-Installer für den Server
#   bastion-<Version>-windows-amd64.zip     Server + Agent ohne Installer
#   bastion-<Version>-linux-amd64.zip       Server + Agent für Linux
#   bastion-<Version>-linux-arm64.zip
#   SHA256SUMS.txt
# Benötigt Go und Inno Setup 6 (https://jrsoftware.org/isdl.php).
param(
    [Parameter(Mandatory = $true)][string]$Version,
    [string]$Iscc = ""
)
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

if ($Version -notmatch '^\d+\.\d+\.\d+$') { throw "Version im Format 1.2.3 angeben" }
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    $goBin = Join-Path $env:ProgramFiles "Go\bin"
    if (-not (Test-Path "$goBin\go.exe")) { throw "Go nicht gefunden" }
    $env:Path = "$goBin;$env:Path"
}
if (-not $Iscc) {
    $Iscc = @(
        "$env:ProgramFiles\Inno Setup 6\ISCC.exe",
        "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe",
        "$env:LOCALAPPDATA\Programs\Inno Setup 6\ISCC.exe"
    ) | Where-Object { Test-Path $_ } | Select-Object -First 1
    if (-not $Iscc) { throw "Inno Setup 6 nicht gefunden (Parameter -Iscc angeben)" }
}

Write-Host "Tests"
go test ./...
if ($LASTEXITCODE -ne 0) { throw "Tests fehlgeschlagen" }

& .\build.ps1 -Version $Version

$out = "dist\release"
if (Test-Path $out) { Remove-Item -Recurse -Force $out }
New-Item -ItemType Directory -Force $out | Out-Null

Write-Host "Installer"
& $Iscc /Q "/DAppVersion=$Version" "/DDistDir=..\dist" "/DOutputDir=..\$out" installer\bastion-server.iss
if ($LASTEXITCODE -ne 0) { throw "Installer-Build fehlgeschlagen" }

foreach ($t in "windows-amd64", "linux-amd64", "linux-arm64") {
    $zip = "$out\bastion-$Version-$t.zip"
    Write-Host "Archiv $zip"
    Compress-Archive -Path "dist\$t\*", "README.md", "wiki.md" -DestinationPath $zip -Force
}

$sums = Get-ChildItem $out -File | Sort-Object Name | ForEach-Object {
    "{0}  {1}" -f (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower(), $_.Name
}
# LF line endings, so "sha256sum -c SHA256SUMS.txt" works on Linux
[IO.File]::WriteAllText((Join-Path (Resolve-Path $out) "SHA256SUMS.txt"), (($sums -join "`n") + "`n"))

Get-ChildItem $out | Format-Table Name, @{ n = 'MB'; e = { [math]::Round($_.Length / 1MB, 1) } }
Write-Host "fertig: .\$out"
