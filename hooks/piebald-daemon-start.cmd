@echo off
REM piebald-daemon-start.cmd - inicia o daemon de memoria se nao estiver rodando.
REM Idempotente: chamado no SessionStart; se ja roda, sai limpo.
REM Lanca destacado (hidden) via PowerShell Start-Process - retorna na hora.
tasklist /FI "IMAGENAME eq piebald-memory-daemon.exe" 2>nul | find /I "piebald-memory-daemon.exe" >nul
if %ERRORLEVEL%==0 exit /b 0
REM Pin do env (2026-06-18): o hook do Piebald roda em cmd /C com env minimo;
REM sem isto o daemon subia com USERPROFILE divergente -> nao lia a OpenRouter
REM key -> embed caia pro BM25. Fixa o home antes de lancar.
set "USERPROFILE=C:\Users\carla"
set "HOME=C:\Users\carla"
powershell -NoProfile -ExecutionPolicy Bypass -Command "Start-Process -FilePath 'C:\Users\carla\bin\piebald-memory-daemon.exe' -WindowStyle Hidden"
exit /b 0
