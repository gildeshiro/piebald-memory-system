@echo off
REM piebald-memory-selector.cmd - UserPromptSubmit hook (versao DAEMON + auth).
REM Encaminha o JSON do hook pro daemon quente e devolve o additionalContext.
REM Envia o X-Daemon-Token (lido do arquivo 0600) pra autenticar no daemon.
REM Se o daemon estiver fora do ar, curl falha -> stdout vazio -> nao injeta
REM (degrade gracioso). exit /b 0 evita ruido de hook-failure no Piebald.
set "TOKFILE=%USERPROFILE%\.claude\.piebald-daemon-token"
set "TOK="
if exist "%TOKFILE%" set /p TOK=<"%TOKFILE%"
curl -s --max-time 6 -X POST http://127.0.0.1:8099/select -H "X-Daemon-Token: %TOK%" --data-binary @-
exit /b 0
