# Windows Service

O próprio binário se registra como serviço do Windows (usa
`golang.org/x/sys/windows/svc`). Rode os comandos num terminal **como
Administrador**.

## Instalar

```powershell
zadodb.exe service install --data-dir "C:\ProgramData\zadodb" --http-addr 127.0.0.1:7373
```

Isso cria o serviço `zadodb` (start automático) apontando para o executável com
o argumento `serve` e as flags fornecidas. O `--data-dir` é resolvido para
caminho absoluto (serviços rodam a partir de um diretório de sistema).

## Gerenciar

```powershell
zadodb.exe service start
zadodb.exe service stop
zadodb.exe service status
zadodb.exe service uninstall
```

Também funcionam os comandos nativos do Windows (`sc.exe query zadodb`,
`services.msc`).

## Como funciona

- Quando o SCM inicia o serviço, o binário detecta que está rodando como serviço
  (`svc.IsWindowsService`) e roda sob o handler do SCM, respondendo a
  Stop/Shutdown com um shutdown gracioso do servidor HTTP.
- Rodar `zadodb serve` num terminal comum roda em foreground com tratamento de
  Ctrl-C.

## Logs

Nesta versão os logs vão para stdout/stderr. Ao rodar como serviço, redirecione
para um arquivo (ex.: configure via um wrapper) ou use o Visualizador de Eventos
conforme sua política. Uma integração de logging estruturado é item de fase
futura.
