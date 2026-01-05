# mensa-localizations

Microservizio Go (Fiber) che fa da **proxy + cache best-effort** per un singolo progetto Tolgee (chiave da env `TOLGEE_APP_KEY`). All’avvio e su webhook ricarica **lingue** e **traduzioni** (flat e nested) in Redis e, se abilitato, su S3.

## Cosa fa
- Espone API HTTP su `:3000` per lingue e traduzioni Tolgee.
- Cache **primaria** in Redis; opzionale **replica** su S3/MinIO (stesse key usate in Redis).
- Warm-up iniziale (se non processo child Fiber) e su webhook `/api/update` con firma Tolgee (`Tolgee-Signature` + `WEBHOOK_SECRET`).
- Se un payload non è in cache: per **lingue** prova Tolgee live e ri-salva in cache; per **traduzioni** tenta fallback `en` dal cache (niente fetch live: serve il warm-up/webhook).

## API
Base URL: `http://localhost:3000`

- `GET /api/healthz` → plain `ok`.
- `GET /api/languages` → JSON lingue Tolgee (cache → S3 → Tolgee live → cache).
- `GET /api/:lang` → traduzioni JSON per `:lang`.
  - Query `nested=true|false` (default `false` flat).
  - Cache → S3; se manca e `:lang` ≠ `en`, ritorna `en` dal cache; se manca anche `en`, errore.
- `ALL /api/update` → webhook Tolgee per rigenerare tutte le cache (lingue + traduzioni flat/nested di ogni lingua).
  - Richiede header `Tolgee-Signature` JSON `{ "timestamp": <ms>, "signature": "<hmac-sha256>" }` firmato con `WEBHOOK_SECRET` sul payload ricevuto.
  - Ritorna `200` se accettato, `401` se firma non valida/assenza secret.
- Catch-all `*` → serve traduzioni `en` dal cache (rispetta `nested=true`).

## Cache
- **Redis**: chiavi `tolgee:languages`, `tolgee:lang:<tag>:<nested>` (`nested` è `true|false`). Nessun TTL (persistenza fino a sovrascrittura).
- **S3/MinIO** (opzionale): usa le stesse chiavi stringa come object key; scrive `Content-Type: application/json`.

## Variabili d’ambiente
- Tolgee: `TOLGEE_APP_KEY` (**required**) chiave progetto; `WEBHOOK_SECRET` (**required** per accettare `/api/update`).
- Redis: `REDIS_ADDR` (default `localhost:6379`), `REDIS_PASSWORD` (default vuota).
- S3/MinIO: `S3_ENABLED` (default `true`), `S3_BUCKET`, `S3_ENDPOINT`, `S3_ACCESS_KEY`, `S3_SECRET_KEY` (**required se S3_ENABLED=true**), `S3_REGION` (default `us-east-1`), `S3_FORCE_PATH_STYLE` (default `true`).
- Debug: `DEBUG=true` per loggare il parse delle env.

## Esecuzione locale
Prerequisiti: Go + Redis in esecuzione (S3 opzionale).

```bash
cd mensa-localizations
go run ./main
```

Servizio su `http://localhost:3000`.

## Docker
Build:
```bash
docker build -t mensa-localizations:local .
```
Run (Redis esterno, S3 disabilitato esempio minimo):
```bash
docker run --rm -p 3000:3000 \
  -e TOLGEE_APP_KEY=<ak> \
  -e REDIS_ADDR=host.docker.internal:6379 \
  -e S3_ENABLED=false \
  -e WEBHOOK_SECRET=mysecret \
  mensa-localizations:local
```
> Su Linux usare l’IP del gateway Docker al posto di `host.docker.internal`.

## Note
- Log verbose con prefissi `[cache]`/`[s3]`/`[webhook]` per osservare hit/miss e firma webhook.
- Se la cache traduzioni non è stata warmata (es. nessun webhook/avvio iniziale fallito), le richieste possono fallire: chiamare `/api/update` con firma valida.
