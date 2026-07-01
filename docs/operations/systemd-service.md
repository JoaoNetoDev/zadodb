# systemd (Linux)

No Linux, o ZadoDB roda em foreground supervisionado pelo systemd
(`Type=simple`).

## Instalar via CLI

```sh
sudo zadodb service install --data-dir /var/lib/zadodb --http-addr 127.0.0.1:7373
sudo systemctl enable --now zadodb
```

`service install` grava `/etc/systemd/system/zadodb.service` e roda
`systemctl daemon-reload` (requer root).

## Gerenciar

```sh
sudo zadodb service start
sudo zadodb service stop
sudo zadodb service status      # (equivale a systemctl status zadodb)
sudo zadodb service uninstall
```

Ou diretamente: `sudo systemctl start|stop|status zadodb`.

## Unit gerada (referência)

```ini
[Unit]
Description=ZadoDB Database Server
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/zadodb serve --data-dir "/var/lib/zadodb" --http-addr "127.0.0.1:7373"
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
```

Ajuste `User=`/`Group=` e permissões do `--data-dir` conforme sua política de
segurança (recomenda-se um usuário de serviço dedicado, dono do diretório de
dados).
