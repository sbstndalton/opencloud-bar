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
; Updating: the running app is stopped cleanly first (PrepareToInstall); Windows' Restart
; Manager closes anything still holding the files. It isn't restarted by Restart Manager:
; the "Start OpenCloud Bar now" option below does that.
CloseApplications=yes
RestartApplications=no
SetupIconFile=..\icon\icon.ico
LicenseFile={#Src}\LICENSE.txt

[Tasks]
; Offered on a first install only: an update leaves the setting as the user has it (Settings
; > General > Start at sign-in), whichever way they set it.
Name: "startup"; Description: "Start OpenCloud Bar when I sign in"; Flags: checkedonce; Check: FirstInstall

[Files]
Source: "{#Src}\OpenCloud Bar.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#Src}\opencloud-bar.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#Src}\rclone.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#Src}\LICENSE.txt"; DestDir: "{app}"
Source: "{#Src}\rclone-LICENSE.txt"; DestDir: "{app}"
Source: "{#Src}\winfsp.msi"; DestDir: "{tmp}"; Flags: deleteafterinstall; Check: not WinFspInstalled

[Icons]
; The app ID lets the app's notifications show as "OpenCloud Bar" (see desktopNotify).
Name: "{group}\OpenCloud Bar"; Filename: "{app}\OpenCloud Bar.exe"; AppUserModelID: "OpenCloudBar.OpenCloudBar"
Name: "{group}\Uninstall OpenCloud Bar"; Filename: "{uninstallexe}"

[Registry]
; Written on a first install with the task ticked; an update never turns the setting back on.
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; ValueName: "OpenCloud Bar"; ValueData: """{app}\OpenCloud Bar.exe"""; Flags: uninsdeletevalue; Tasks: startup; Check: FirstInstall
; No task and no Check, so this is processed (and logged for the uninstaller) on every install:
; uninstalling removes the value even when the task wasn't ticked, e.g. the app's own setting
; turned it on later. It creates no value and leaves an existing one alone.
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: none; ValueName: "OpenCloud Bar"; Flags: uninsdeletevalue
; Explorer's right-click menu on a folder: "Keep on this PC with OpenCloud Bar" runs
; `opencloud-bar keep-drive "<folder>"`, which keeps it when it is inside the OpenCloud drive and
; says so when it isn't. Per user (HKCU\Software\Classes); uninsdeletekey removes the whole verb
; key (its command included) when uninstalling.
Root: HKCU; Subkey: "Software\Classes\Directory\shell\OpenCloudBarKeep"; ValueType: string; ValueName: ""; ValueData: "Keep on this PC with OpenCloud Bar"; Flags: uninsdeletekey
Root: HKCU; Subkey: "Software\Classes\Directory\shell\OpenCloudBarKeep"; ValueType: string; ValueName: "Icon"; ValueData: """{app}\OpenCloud Bar.exe"""
Root: HKCU; Subkey: "Software\Classes\Directory\shell\OpenCloudBarKeep\command"; ValueType: string; ValueName: ""; ValueData: """{app}\opencloud-bar.exe"" keep-drive ""%V"""
Root: HKLM; Subkey: "SYSTEM\CurrentControlSet\Control\Session Manager\Environment"; ValueType: expandsz; ValueName: "Path"; ValueData: "{olddata};{app}"; Check: NeedsAddPath(ExpandConstant('{app}'))

[Run]
Filename: "msiexec.exe"; Parameters: "/i ""{tmp}\winfsp.msi"" /qn /norestart"; StatusMsg: "Installing WinFsp (needed for the OpenCloud drive)..."; Flags: waituntilterminated; Check: not WinFspInstalled
Filename: "{app}\OpenCloud Bar.exe"; Description: "Start OpenCloud Bar now"; Flags: nowait postinstall skipifsilent runasoriginaluser

[UninstallRun]
; The app was stopped in InitializeUninstall (only once nothing is uploading). Forget the sign-in
; (the stored refresh token only; settings and kept folders stay).
Filename: "{app}\opencloud-bar.exe"; Parameters: "logout --force"; Flags: runhidden waituntilterminated; RunOnceId: "Logout"

[Code]
// Whether an earlier install exists (the uninstall key Inno writes for this AppId; this setup
// runs in 64-bit mode, where HKLM is the 64-bit view the key is in). Read once, in
// InitializeSetup, before this run writes anything: later it would always be found.
var PreviousInstall: Boolean;

function InitializeSetup(): Boolean;
begin
  PreviousInstall := RegKeyExists(HKLM, 'Software\Microsoft\Windows\CurrentVersion\Uninstall\{6B7C0E2B-3D7E-4F0A-9C55-0C1B2A7F4E21}_is1');
  Result := True;
end;

function FirstInstall: Boolean;
begin
  Result := not PreviousInstall;
end;

// WinFsp's installer records InstallDir under HKLM\SOFTWARE\WinFsp in the 32-bit registry view,
// which on 64-bit Windows is HKLM\SOFTWARE\WOW6432Node\WinFsp. (rclone's FUSE layer, cgofuse,
// looks it up the same way: Software\WinFsp with RRF_SUBKEY_WOW6432KEY.) This setup runs in
// 64-bit mode, where plain HKLM means the 64-bit view, so the 32-bit view must be named:
// HKLM32. HKLM64 is checked too in case a future WinFsp writes there. The DLL must exist as well:
// a leftover key after a broken uninstall doesn't count.
function WinFspAt(Root: Integer): Boolean;
var Dir: String;
begin
  Result := RegQueryStringValue(Root, 'SOFTWARE\WinFsp', 'InstallDir', Dir) and
            FileExists(AddBackslash(Dir) + 'bin\winfsp-x64.dll');
end;

function WinFspInstalled: Boolean;
begin
  Result := WinFspAt(HKLM32) or WinFspAt(HKLM64);
end;

// `opencloud-bar quit` exits 2 when it refused because files written to the drive may still be
// uploading (0: stopped or not running). Installer and uninstaller never force it: they stop
// and say so, as the README's Safety section promises.
const UploadsPendingMsg = 'OpenCloud Bar is still uploading files written to the OpenCloud drive (or can''t tell whether it is). ' +
  'Wait a moment, or quit it from its tray icon (it asks before quitting), then try again.';

// Before files are replaced on an update: stop the running app cleanly (as the signed-in
// user, whose app it is), so the drive is ejected and a running sync saves its state.
function PrepareToInstall(var NeedsRestart: Boolean): String;
var Code: Integer;
begin
  Result := '';
  if FileExists(ExpandConstant('{app}\opencloud-bar.exe')) then
    if ExecAsOriginalUser(ExpandConstant('{app}\opencloud-bar.exe'), 'quit', '', SW_HIDE, ewWaitUntilTerminated, Code) and (Code = 2) then
      Result := UploadsPendingMsg + ' Then choose Back and Next.';
end;

function InitializeUninstall(): Boolean;
var Code: Integer;
begin
  Result := True;
  if Exec(ExpandConstant('{app}\opencloud-bar.exe'), 'quit', '', SW_HIDE, ewWaitUntilTerminated, Code) and (Code = 2) then
  begin
    Log('Uninstall stopped: ' + UploadsPendingMsg);
    if not UninstallSilent then
      MsgBox(UploadsPendingMsg + ' Then uninstall again.', mbInformation, MB_OK);
    Result := False;
  end;
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
