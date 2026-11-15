; Windows installer: OpenCloud Bar + rclone + WinFsp (for the drive letter), Start menu entry,
; optional start at sign-in, and `opencloud-bar` on PATH for the command line.
; Build: iscc /DAppVersion=1.2.3 /DSrc=<folder with the files> opencloud-bar.iss

#ifndef AppVersion
  #define AppVersion "0.0.0"
#endif
#ifndef Src
  #define Src "..\..\dist-win"
#endif

[Setup]
AppId={{6B7C0E2B-3D7E-4F0A-9C55-0C1B2A7F4E21}
AppName=OpenCloud Bar
AppVersion={#AppVersion}
AppPublisher=OpenCloud Bar contributors
AppPublisherURL=https://github.com/sbstndalton/opencloud-bar
DefaultDirName={autopf}\OpenCloud Bar
DefaultGroupName=OpenCloud Bar
DisableProgramGroupPage=yes
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
ChangesEnvironment=yes
OutputDir={#Src}\out
OutputBaseFilename=OpenCloud-Bar-Setup-{#AppVersion}
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
UninstallDisplayIcon={app}\OpenCloud Bar.exe
SetupIconFile=..\icon\icon.ico
LicenseFile={#Src}\LICENSE.txt

[Tasks]
Name: "startup"; Description: "Start OpenCloud Bar when I sign in"; Flags: checkedonce

[Files]
Source: "{#Src}\OpenCloud Bar.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#Src}\opencloud-bar.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#Src}\rclone.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#Src}\LICENSE.txt"; DestDir: "{app}"
Source: "{#Src}\rclone-LICENSE.txt"; DestDir: "{app}"
Source: "{#Src}\winfsp.msi"; DestDir: "{tmp}"; Flags: deleteafterinstall; Check: not WinFspInstalled

[Icons]
Name: "{group}\OpenCloud Bar"; Filename: "{app}\OpenCloud Bar.exe"
Name: "{group}\Uninstall OpenCloud Bar"; Filename: "{uninstallexe}"

[Registry]
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; ValueName: "OpenCloud Bar"; ValueData: """{app}\OpenCloud Bar.exe"""; Flags: uninsdeletevalue; Tasks: startup
Root: HKLM; Subkey: "SYSTEM\CurrentControlSet\Control\Session Manager\Environment"; ValueType: expandsz; ValueName: "Path"; ValueData: "{olddata};{app}"; Check: NeedsAddPath(ExpandConstant('{app}'))

[Run]
Filename: "msiexec.exe"; Parameters: "/i ""{tmp}\winfsp.msi"" /qn /norestart"; StatusMsg: "Installing WinFsp (needed for the OpenCloud drive)..."; Flags: waituntilterminated; Check: not WinFspInstalled
Filename: "{app}\OpenCloud Bar.exe"; Description: "Start OpenCloud Bar now"; Flags: nowait postinstall skipifsilent runasoriginaluser

[UninstallRun]
Filename: "{app}\opencloud-bar.exe"; Parameters: "quit"; Flags: runhidden; RunOnceId: "QuitApp"

[Code]
function WinFspInstalled: Boolean;
var Dir: String;
begin
  Result := RegQueryStringValue(HKLM32, 'SOFTWARE\WinFsp', 'InstallDir', Dir) and DirExists(Dir);
end;

function NeedsAddPath(Param: String): Boolean;
var Paths: String;
begin
  if not RegQueryStringValue(HKLM, 'SYSTEM\CurrentControlSet\Control\Session Manager\Environment', 'Path', Paths) then
  begin
    Result := True;
    exit;
  end;
  Result := Pos(';' + Uppercase(Param) + ';', ';' + Uppercase(Paths) + ';') = 0;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var Paths, App: String; P: Integer;
begin
  if CurUninstallStep = usPostUninstall then
  begin
    App := ExpandConstant('{app}');
    if RegQueryStringValue(HKLM, 'SYSTEM\CurrentControlSet\Control\Session Manager\Environment', 'Path', Paths) then
    begin
      P := Pos(';' + Uppercase(App), Uppercase(Paths));
      if P > 0 then
      begin
        Delete(Paths, P, Length(App) + 1);
        RegWriteExpandStringValue(HKLM, 'SYSTEM\CurrentControlSet\Control\Session Manager\Environment', 'Path', Paths);
      end;
    end;
  end;
end;
