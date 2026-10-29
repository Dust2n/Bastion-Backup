# Builds server and agents for Windows and Linux into .\dist.
# Optional: -Publish <Datenverzeichnis des Servers> copies the agents to
# <data>\agents\<os>-<arch>\ so the server can distribute and update them.
param(
    [string]$Version = "0.1.0",
    [string]$Publish = ""
)
$ErrorActionPreference = "Stop"
$env:CGO_ENABLED = "0"
$ldflags = "-s -w -X bastion/internal/proto.Version=$Version"
$targets = @(
    @{ os = "windows"; arch = "amd64"; ext = ".exe" },
    @{ os = "linux";   arch = "amd64"; ext = "" },
    @{ os = "linux";   arch = "arm64"; ext = "" }
)
foreach ($t in $targets) {
    $dir = "dist\$($t.os)-$($t.arch)"
    New-Item -ItemType Directory -Force $dir | Out-Null
    $env:GOOS = $t.os; $env:GOARCH = $t.arch
    Write-Host "baue $($t.os)/$($t.arch)"
    go build -trimpath -ldflags $ldflags -o "$dir\bastion-agent$($t.ext)" ./cmd/bastion-agent
    if ($LASTEXITCODE -ne 0) { throw "Build fehlgeschlagen" }
    go build -trimpath -ldflags $ldflags -o "$dir\bastion-server$($t.ext)" ./cmd/bastion-server
    if ($LASTEXITCODE -ne 0) { throw "Build fehlgeschlagen" }
    Set-Content "$dir\VERSION" $Version -Encoding ascii
    if ($Publish) {
        $dst = Join-Path $Publish "agents\$($t.os)-$($t.arch)"
        New-Item -ItemType Directory -Force $dst | Out-Null
        Copy-Item "$dir\bastion-agent$($t.ext)", "$dir\VERSION" $dst -Force
    }
}
Remove-Item Env:GOOS, Env:GOARCH
Write-Host "fertig: .\dist"
