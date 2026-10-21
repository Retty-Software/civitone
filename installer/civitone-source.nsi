; Civitone Source installer (NSIS). Built by `make installer`:
;   makensis -DVERSION=... -DSRCEXE=path\to\civitone-source.exe -DOUTFILE=... civitone-source.nsi
;
; Per-user install: no admin prompt, which matters on borrowed festival
; laptops. Starts at login (HKCU Run), Start menu shortcut, Apps & features
; entry. Settings and adoption tokens live in %APPDATA%\Civitone and survive
; uninstall and upgrade.

Unicode true
!include "MUI2.nsh"

!define APP "Civitone Source"
!define EXE "civitone-source.exe"
!define UNINST "Software\Microsoft\Windows\CurrentVersion\Uninstall\CivitoneSource"
!define RUNKEY "Software\Microsoft\Windows\CurrentVersion\Run"
!ifndef VERSION
  !define VERSION "dev"
!endif
!ifndef SRCEXE
  !define SRCEXE "..\dist\civitone-source-gui.exe"
!endif
!ifndef OUTFILE
  !define OUTFILE "..\dist\CivitoneSource-Setup.exe"
!endif

Name "${APP}"
OutFile "${OUTFILE}"
RequestExecutionLevel user
InstallDir "$LOCALAPPDATA\Programs\Civitone Source"
SetCompressor /SOLID lzma
VIProductVersion "0.0.0.0"
VIAddVersionKey "ProductName" "${APP}"
VIAddVersionKey "ProductVersion" "${VERSION}"
VIAddVersionKey "FileDescription" "${APP} installer"
VIAddVersionKey "FileVersion" "${VERSION}"
VIAddVersionKey "LegalCopyright" "Civitone contributors"

!define MUI_ICON "icon.ico"
!define MUI_UNICON "icon.ico"
!define MUI_WELCOMEPAGE_TEXT "Civitone Source sends audio from this computer to your Civitone server: whatever this computer plays, an audio input, or a test chime.$\r$\n$\r$\nIt starts when you sign in and lives in the tray. After installing, its setup page opens in your browser."
!define MUI_FINISHPAGE_RUN "$INSTDIR\${EXE}"
!define MUI_FINISHPAGE_RUN_PARAMETERS "-open"
!define MUI_FINISHPAGE_RUN_TEXT "Start Civitone Source and open its setup page"
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

Section "Install"
  ; Replace a running copy (upgrade).
  nsExec::Exec 'taskkill /IM ${EXE} /F'
  Sleep 800
  SetOutPath "$INSTDIR"
  File "/oname=${EXE}" "${SRCEXE}"
  File "icon.ico"
  WriteUninstaller "$INSTDIR\uninstall.exe"

  CreateShortcut "$SMPROGRAMS\${APP}.lnk" "$INSTDIR\${EXE}" "-open" "$INSTDIR\icon.ico"
  WriteRegStr HKCU "${RUNKEY}" "${APP}" '"$INSTDIR\${EXE}"'

  WriteRegStr HKCU "${UNINST}" "DisplayName" "${APP}"
  WriteRegStr HKCU "${UNINST}" "DisplayVersion" "${VERSION}"
  WriteRegStr HKCU "${UNINST}" "Publisher" "Civitone"
  WriteRegStr HKCU "${UNINST}" "DisplayIcon" "$INSTDIR\icon.ico"
  WriteRegStr HKCU "${UNINST}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${UNINST}" "UninstallString" '"$INSTDIR\uninstall.exe"'
  WriteRegStr HKCU "${UNINST}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /S'
  WriteRegDWORD HKCU "${UNINST}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINST}" "NoRepair" 1
SectionEnd

Section "Uninstall"
  nsExec::Exec 'taskkill /IM ${EXE} /F'
  Sleep 800
  Delete "$INSTDIR\${EXE}"
  Delete "$INSTDIR\icon.ico"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR"
  Delete "$SMPROGRAMS\${APP}.lnk"
  DeleteRegValue HKCU "${RUNKEY}" "${APP}"
  DeleteRegKey HKCU "${UNINST}"
  ; %APPDATA%\Civitone (settings, tokens) is kept on purpose: reinstalling
  ; picks up where it left off. Forget the sources in the console to revoke.
SectionEnd
