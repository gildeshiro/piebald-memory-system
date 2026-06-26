@echo off
REM piebald-daemon-start.cmd - inicia o daemon de memoria se nao estiver rodando.
REM Idempotente: chamado no SessionStart; se ja roda, sai limpo.
REM Lanca destacado (hidden) via PowerShell Start-Process - retorna na hora.
tasklist /FI "IMAGENAME eq piebald-memory-daemon.exe" 2>nul | find /I "piebald-memory-daemon.exe" >nul
if %ERRORLEVEL%==0 exit /b 0
powershell -NoProfile -ExecutionPolicy Bypass -Command "Start-Process -FilePath '%USERPROFILE%\bin\piebald-memory-daemon.exe' -WindowStyle Hidden"
exit /b 0
