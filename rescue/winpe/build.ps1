<#
.SYNOPSIS
  Erstellt die Bastion-Wiederherstellungsumgebung (WinPE).

.DESCRIPTION
  Voraussetzung: Windows ADK und das WinPE-Add-on (https://learn.microsoft.com/windows-hardware/get-started/adk-install).
  Ausführen in einer PowerShell als Administrator.

  Ergebnis in -Out:
    winpe.wim                  für die Wiederherstellung im laufenden Betrieb und für PXE
    bastion-rescue-winpe.iso   für USB-Stick (z. B. mit Rufus schreiben) oder virtuelle Maschinen

  Wird -Out auf <Serverdaten>\rescue gesetzt, stehen die Dateien sofort im Server bereit.

.EXAMPLE
  .\build.ps1 -Server https://backup01:8443 -Fingerprint 7cca21... -Agent ..\..\dist\windows-amd64\bastion-agent.exe -Out C:\ProgramData\BastionServer\rescue
#>
param(
    [Parameter(Mandatory)] [string]$Server,
    [Parameter(Mandatory)] [string]$Fingerprint,
    [Parameter(Mandatory)] [string]$Agent,
    [string]$Out = ".\out",
    [string]$Keyboard = "0407:00000407",
    [string]$Drivers = ""
)
$ErrorActionPreference = 'Stop'
$adk = "${env:ProgramFiles(x86)}\Windows Kits\10\Assessment and Deployment Kit"
$peRoot = "$adk\Windows Preinstallation Environment"
$pe = "$peRoot\amd64"
if (-not (Test-Path "$pe\en-us\winpe.wim")) { throw "WinPE-Add-on des Windows ADK nicht gefunden ($pe)" }
if (-not (Test-Path $Agent)) { throw "Agent-Programm nicht gefunden: $Agent" }

# copype fails on long or 8.3 temp paths, so use a short folder
$work = Join-Path "$env:SystemDrive\" "BastionPE-$(Get-Random)"
New-Item -ItemType Directory -Force $Out | Out-Null
$Out = (Resolve-Path $Out).Path

Write-Host "Kopiere WinPE nach $work"
cmd /c "`"$adk\Deployment Tools\DandISetEnv.bat`" && copype amd64 `"$work`"" | Out-Null
if ($LASTEXITCODE -ne 0) { throw "copype fehlgeschlagen" }
$mount = "$work\mount"
$wim = "$work\media\sources\boot.wim"

Write-Host "Binde boot.wim ein"
dism /Mount-Image /ImageFile:"$wim" /Index:1 /MountDir:"$mount" /Quiet
if ($LASTEXITCODE -ne 0) { throw "DISM Mount fehlgeschlagen" }
try {
    # Optional packages (PowerShell and WMI for diagnosis in the console).
    # Bastion itself needs none of them, so failures only produce a warning.
    $failed = @()
    foreach ($p in 'WinPE-WMI', 'WinPE-NetFX', 'WinPE-Scripting', 'WinPE-PowerShell', 'WinPE-StorageWMI', 'WinPE-DismCmdlets', 'WinPE-EnhancedStorage') {
        Write-Host "  Paket $p"
        dism /Image:"$mount" /Add-Package /PackagePath:"$pe\WinPE_OCs\$p.cab" /Quiet | Out-Null
        if ($LASTEXITCODE -ne 0) { $failed += $p; continue }
        foreach ($lang in 'de-de', 'en-us') {
            $lp = "$pe\WinPE_OCs\$lang\${p}_$lang.cab"
            if (Test-Path $lp) { dism /Image:"$mount" /Add-Package /PackagePath:"$lp" /Quiet | Out-Null; break }
        }
    }
    if ($failed) {
        Write-Warning ("Optionale WinPE-Pakete nicht hinzugefügt: " + ($failed -join ', ') +
            ". Bastion funktioniert trotzdem. (Häufige Ursache: Ausführung in einer eingeschränkten Umgebung, die das Laden von Registry-Hives verhindert – Skript in einer normalen Administrator-PowerShell starten.)")
    }
    if ($Drivers) {
        Write-Host "  Treiber aus $Drivers"
        dism /Image:"$mount" /Add-Driver /Driver:"$Drivers" /Recurse /ForceUnsigned /Quiet | Out-Null
        if ($LASTEXITCODE -ne 0) { Write-Warning "Treiber konnten nicht hinzugefügt werden" }
    }
    dism /Image:"$mount" /Set-InputLocale:$Keyboard /Quiet | Out-Null
    if ($LASTEXITCODE -ne 0) { Write-Warning "Tastaturlayout nicht im Abbild gesetzt; startnet.cmd setzt es beim Start (wpeutil SetKeyboardLayout)" }

    # German keyboard: the layout file must be in the image
    if (-not (Test-Path "$mount\Windows\System32\kbdgr.dll")) {
        throw "kbdgr.dll (deutsches Tastaturlayout) fehlt im WinPE-Abbild"
    }
    # Bastion background instead of the default black one
    $bg = Join-Path $PSScriptRoot "winpe.jpg"
    if (Test-Path $bg) {
        $dst = "$mount\Windows\System32\winpe.jpg"
        takeown /f $dst | Out-Null
        icacls $dst /grant "*S-1-5-32-544:F" | Out-Null
        Copy-Item $bg $dst -Force
    }

    New-Item -ItemType Directory -Force "$mount\bastion" | Out-Null
    Copy-Item $Agent "$mount\bastion\bastion-agent.exe"
    @{ server = $Server; fingerprint = $Fingerprint } | ConvertTo-Json | Set-Content "$mount\bastion\rescue.json" -Encoding ascii
    @"
@echo off
wpeinit
wpeutil SetKeyboardLayout $Keyboard >nul 2>&1
powercfg /s 8c5e7fda-e8bf-4a96-9a85-a6e23a8c635c >nul 2>&1
title Bastion Wiederherstellung
color 1F
mode con cols=120 lines=40 >nul 2>&1
X:\bastion\bastion-agent.exe rescue --auto
echo.
echo Bastion beendet. Eingabeaufforderung fuer Diagnose (wpeutil reboot = Neustart).
cmd /k
"@ | Set-Content "$mount\Windows\System32\startnet.cmd" -Encoding ascii
} catch {
    dism /Unmount-Image /MountDir:"$mount" /Discard /Quiet | Out-Null
    throw
}
Write-Host "Speichere boot.wim"
dism /Unmount-Image /MountDir:"$mount" /Commit /Quiet
if ($LASTEXITCODE -ne 0) { throw "DISM Unmount fehlgeschlagen" }

Copy-Item $wim "$Out\winpe.wim" -Force
Write-Host "Erzeuge ISO"
cmd /c "`"$adk\Deployment Tools\DandISetEnv.bat`" && MakeWinPEMedia /ISO /F `"$work`" `"$Out\bastion-rescue-winpe.iso`"" | Out-Null
Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
Write-Host "Fertig:"
Get-ChildItem "$Out\winpe.wim", "$Out\bastion-rescue-winpe.iso" | Format-Table Name, @{n = 'MB'; e = { [math]::Round($_.Length / 1MB) } }
