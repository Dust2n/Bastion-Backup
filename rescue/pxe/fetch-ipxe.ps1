<#
.SYNOPSIS
  Lädt iPXE und wimboot für den Netzwerkstart in das Serververzeichnis.
.EXAMPLE
  .\fetch-ipxe.ps1 -Out C:\ProgramData\BastionServer\pxe
#>
param([string]$Out = "C:\ProgramData\BastionServer\pxe")
$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
New-Item -ItemType Directory -Force $Out | Out-Null
$files = @{
    'ipxe.efi'      = 'https://boot.ipxe.org/ipxe.efi'
    'undionly.kpxe' = 'https://boot.ipxe.org/undionly.kpxe'
    'wimboot'       = 'https://github.com/ipxe/wimboot/releases/latest/download/wimboot'
}
foreach ($f in $files.Keys) {
    Write-Host "Lade $f"
    Invoke-WebRequest -UseBasicParsing -Uri $files[$f] -OutFile (Join-Path $Out $f)
}
Get-ChildItem $Out | Format-Table Name, Length
Write-Host "PXE danach in der Weboberfläche unter Einstellungen aktivieren und den Server neu starten."
