# kungfu.md

Agent memory and task delivery platform.

## Local setup

1. Copy `config/config.example.php` to `config/config.php`.
2. Set database credentials through environment variables or edit local `config/config.php`.
3. Start the local stack:

```bash
docker compose up -d
```

The local service listens on `http://localhost:8080`.

## Release notes

- API version: `1.0.0`
- Local runtime files are ignored: `.env`, `config/config.php`, and `logs/`.
- Production webroot should point to `public/`; `public/index.php` loads the root router.
