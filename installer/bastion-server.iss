; Windows-Installer für den Bastion-Backup-Server (Inno Setup 6).
;
; Gebaut wird er von release.ps1:
;   iscc /DAppVersion=0.3.1 /DDistDir=..\dist /DOutputDir=..\dist\release installer\bastion-server.iss
;
; Der Installer
;   - installiert bastion-server.exe nach "C:\Program Files\Bastion Backup Server",
;   - legt das Datenverzeichnis an und beschränkt es auf SYSTEM und Administratoren
;     (dort liegen repo.key, die Datenbank und der TLS-Schlüssel),
;   - stellt die Agent-Programme unter <Daten>\agents bereit (Verteilung und Updates),
;   - richtet den Windows-Dienst "BastionServer" ein und startet ihn,
;   - öffnet auf Wunsch den Port in der Windows-Firewall.
; Bei einem Update werden Dienst und Einstellungen übernommen. Beim Deinstallieren
; bleibt das Datenverzeichnis mit allen Backups und Schlüsseln erhalten.
;
; Unbeaufsichtigt: BastionServer-Setup-<Version>.exe /VERYSILENT /DATADIR="D:\BastionData" /PORT=8443

#ifndef AppVersion
  #define AppVersion "0.0.0"
#endif
#ifndef DistDir
  #define DistDir "..\dist"
#endif
#ifndef OutputDir
  #define OutputDir "..\dist\release"
#endif

#define AppName "Bastion Backup Server"
#define ServiceName "BastionServer"
#define RegKey "Software\Bastion Backup Server"
#define FirewallRule "Bastion Backup Server"

[Setup]
AppId={{6F1C2B7E-4A3D-4E8B-9C21-B7A5D0E3F914}
AppName={#AppName}
AppVersion={#AppVersion}
AppVerName={#AppName} {#AppVersion}
AppPublisher=Bastion Backup
AppSupportURL=https://github.com/Dust2n/Bastion-Backup
VersionInfoVersion={#AppVersion}
VersionInfoProductName={#AppName}
DefaultDirName={autopf}\{#AppName}
DisableProgramGroupPage=yes
DisableDirPage=auto
UsePreviousAppDir=yes
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
WizardStyle=modern
Compression=lzma2/max
SolidCompression=yes
OutputDir={#OutputDir}
OutputBaseFilename=BastionServer-Setup-{#AppVersion}
UninstallDisplayName={#AppName}
UninstallDisplayIcon={app}\bastion-server.exe
CloseApplications=no
SetupLogging=yes

[Languages]
Name: "de"; MessagesFile: "compiler:Languages\German.isl"

[Tasks]
Name: "firewall"; Description: "Windows-Firewall: eingehende Verbindungen auf den Server-Port erlauben"

[Files]
Source: "{#DistDir}\windows-amd64\bastion-server.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#DistDir}\windows-amd64\VERSION"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\README.md"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\wiki.md"; DestDir: "{app}"; Flags: ignoreversion
; Agents: werden nach der Installation nach <Daten>\agents kopiert
Source: "{#DistDir}\windows-amd64\bastion-agent.exe"; DestDir: "{app}\agents\windows-amd64"; Flags: ignoreversion
Source: "{#DistDir}\windows-amd64\VERSION"; DestDir: "{app}\agents\windows-amd64"; Flags: ignoreversion
Source: "{#DistDir}\linux-amd64\bastion-agent"; DestDir: "{app}\agents\linux-amd64"; Flags: ignoreversion
Source: "{#DistDir}\linux-amd64\VERSION"; DestDir: "{app}\agents\linux-amd64"; Flags: ignoreversion
Source: "{#DistDir}\linux-arm64\bastion-agent"; DestDir: "{app}\agents\linux-arm64"; Flags: ignoreversion
Source: "{#DistDir}\linux-arm64\VERSION"; DestDir: "{app}\agents\linux-arm64"; Flags: ignoreversion
; Skripte für die Rettungssysteme (WinPE, Linux, PXE)
Source: "..\rescue\*"; DestDir: "{app}\rescue"; Flags: ignoreversion recursesubdirs

[Registry]
Root: HKLM; Subkey: "{#RegKey}"; ValueType: string; ValueName: "DataDir"; ValueData: "{code:GetDataDir}"; Flags: uninsdeletekey
Root: HKLM; Subkey: "{#RegKey}"; ValueType: string; ValueName: "Port"; ValueData: "{code:GetPort}"
Root: HKLM; Subkey: "{#RegKey}"; ValueType: string; ValueName: "Version"; ValueData: "{#AppVersion}"

[Run]
Filename: "https://localhost:{code:GetPort}/"; Description: "Weboberfläche öffnen"; Flags: postinstall shellexec nowait skipifsilent

[Code]
var
  DataPage: TInputDirWizardPage;
  PortPage: TInputQueryWizardPage;
  Upgrade: Boolean;
  ServiceError: String;
  UninstData: String;

function ServiceExists: Boolean;
begin
  Result := RegKeyExists(HKLM, 'SYSTEM\CurrentControlSet\Services\{#ServiceName}');
end;

function PreviousValue(Name, Default: String): String;
begin
  if not RegQueryStringValue(HKLM, '{#RegKey}', Name, Result) or (Result = '') then
    Result := Default;
end;

function GetDataDir(Param: String): String;
begin
  Result := RemoveBackslashUnlessRoot(Trim(DataPage.Values[0]));
end;

function GetPort(Param: String): String;
begin
  Result := Trim(PortPage.Values[0]);
end;

procedure InitializeWizard;
begin
  Upgrade := ServiceExists;
  DataPage := CreateInputDirPage(wpSelectDir, 'Datenverzeichnis',
    'Wo sollen Datenbank, Schlüssel und das Standard-Backup-Repository liegen?',
    'Weitere Speicherorte (andere Laufwerke, NAS) lassen sich später in der Weboberfläche anlegen.' + #13#10 +
    'Wichtig: Den Unterordner "keys" sicher aufbewahren – ohne repo.key sind die Backups nicht lesbar.',
    False, '');
  DataPage.Add('Datenverzeichnis:');
  DataPage.Values[0] := ExpandConstant('{param:DATADIR|' +
    PreviousValue('DataDir', ExpandConstant('{commonappdata}\BastionServer')) + '}');

  PortPage := CreateInputQueryPage(DataPage.ID, 'Netzwerk',
    'Unter welchem Port ist der Server erreichbar?',
    'Weboberfläche und Agents verbinden sich per HTTPS auf diesen Port.');
  PortPage.Add('HTTPS-Port:', False);
  PortPage.Values[0] := ExpandConstant('{param:PORT|' + PreviousValue('Port', '8443') + '}');
end;

function ShouldSkipPage(PageID: Integer): Boolean;
begin
  // the existing service keeps its data directory and port; ask only if
  // this installer does not know them (service set up by hand)
  Result := Upgrade and RegValueExists(HKLM, '{#RegKey}', 'DataDir') and
    ((PageID = DataPage.ID) or (PageID = PortPage.ID));
end;

function NextButtonClick(CurPageID: Integer): Boolean;
var
  P: Integer;
begin
  Result := True;
  if CurPageID = DataPage.ID then begin
    if (Length(GetDataDir('')) < 3) or (Pos(':\', GetDataDir('')) <> 2) then begin
      MsgBox('Bitte einen absoluten lokalen Pfad angeben, z. B. D:\BastionData.', mbError, MB_OK);
      Result := False;
    end;
  end;
  if CurPageID = PortPage.ID then begin
    P := StrToIntDef(GetPort(''), 0);
    if (P < 1) or (P > 65535) then begin
      MsgBox('Bitte einen Port zwischen 1 und 65535 angeben.', mbError, MB_OK);
      Result := False;
    end;
  end;
end;

function Run(Exe, Params: String): Integer;
begin
  if not Exec(Exe, Params, '', SW_HIDE, ewWaitUntilTerminated, Result) then
    Result := -1;
  Log(Format('%s %s -> %d', [Exe, Params, Result]));
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  Result := '';
  if ServiceExists then begin
    // the running server holds bastion-server.exe open
    Run(ExpandConstant('{sys}\net.exe'), 'stop {#ServiceName}');
    Sleep(1500);
  end;
end;

procedure CopyAgents(Data: String);
var
  Arch: array[0..2] of String;
  Names: array[0..2] of String;
  I: Integer;
  Src, Dst: String;
begin
  Arch[0] := 'windows-amd64'; Names[0] := 'bastion-agent.exe';
  Arch[1] := 'linux-amd64';   Names[1] := 'bastion-agent';
  Arch[2] := 'linux-arm64';   Names[2] := 'bastion-agent';
  for I := 0 to 2 do begin
    Src := ExpandConstant('{app}\agents\') + Arch[I] + '\';
    Dst := Data + '\agents\' + Arch[I] + '\';
    ForceDirectories(Dst);
    if not FileCopy(Src + Names[I], Dst + Names[I], False) then
      Log('Agent nicht kopiert: ' + Src + Names[I]);
    FileCopy(Src + 'VERSION', Dst + 'VERSION', False);
  end;
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  Data, Port, Sys: String;
  Code: Integer;
begin
  if CurStep <> ssPostInstall then
    exit;
  Data := GetDataDir('');
  Port := GetPort('');
  Sys := ExpandConstant('{sys}');

  // Keys, database and TLS key: only SYSTEM (S-1-5-18) and Administrators
  // (S-1-5-32-544). ProgramData would otherwise let every local user read them.
  ForceDirectories(Data);
  Run(Sys + '\icacls.exe', '"' + Data + '" /inheritance:r /grant:r *S-1-5-18:(OI)(CI)F *S-1-5-32-544:(OI)(CI)F');

  CopyAgents(Data);

  if WizardIsTaskSelected('firewall') then begin
    Run(Sys + '\netsh.exe', 'advfirewall firewall delete rule name="{#FirewallRule}"');
    Run(Sys + '\netsh.exe', 'advfirewall firewall add rule name="{#FirewallRule}" dir=in action=allow protocol=TCP localport=' + Port +
      ' program="' + ExpandConstant('{app}\bastion-server.exe') + '"');
  end;

  // creates the service, or starts the existing one
  Code := Run(ExpandConstant('{app}\bastion-server.exe'), '-install-service -data "' + Data + '" -listen :' + Port);
  if Code <> 0 then
    ServiceError := Format('Der Dienst konnte nicht eingerichtet werden (Fehler %d). Details: %s\server.log', [Code, Data]);
end;

procedure CurPageChanged(CurPageID: Integer);
var
  Pw: AnsiString;
  PwFile, Text: String;
  I: Integer;
begin
  if CurPageID <> wpFinished then
    exit;
  Text := 'Der Bastion-Backup-Server läuft als Windows-Dienst "{#ServiceName}".' + #13#10#13#10 +
    'Weboberfläche: https://' + GetComputerNameString + ':' + GetPort('') + #13#10 +
    '(Der Browser warnt beim ersten Aufruf vor dem selbstsignierten Zertifikat.)' + #13#10#13#10;
  PwFile := GetDataDir('') + '\initial-admin-password.txt';
  // the server writes the file at its first start
  for I := 1 to 20 do begin
    if Upgrade or FileExists(PwFile) then
      break;
    Sleep(500);
  end;
  if LoadStringFromFile(PwFile, Pw) then
    Text := Text + 'Erste Anmeldung:' + #13#10 + String(Pw) + #13#10 +
      'Das Passwort bitte nach der Anmeldung ändern; die Datei wird dann gelöscht.' + #13#10#13#10;
  Text := Text + 'Danach unter Einstellungen die Server-Adresse eintragen, unter der die Rechner den Server erreichen.';
  if ServiceError <> '' then
    Text := ServiceError;
  WizardForm.FinishedLabel.Caption := Text;
  WizardForm.FinishedLabel.AutoSize := True;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usUninstall then begin
    // read before the registry key is removed
    RegQueryStringValue(HKLM, '{#RegKey}', 'DataDir', UninstData);
    Run(ExpandConstant('{sys}\net.exe'), 'stop {#ServiceName}');
    Run(ExpandConstant('{sys}\sc.exe'), 'delete {#ServiceName}');
    Run(ExpandConstant('{sys}\netsh.exe'), 'advfirewall firewall delete rule name="{#FirewallRule}"');
  end;
  if CurUninstallStep = usPostUninstall then begin
    if (UninstData <> '') and not UninstallSilent then
      MsgBox('Das Datenverzeichnis wurde nicht gelöscht:' + #13#10 + UninstData + #13#10#13#10 +
        'Es enthält Datenbank, Schlüssel und Backups. Bei Bedarf von Hand löschen – ohne den Ordner "keys" sind die Backups nicht mehr lesbar.',
        mbInformation, MB_OK);
  end;
end;
