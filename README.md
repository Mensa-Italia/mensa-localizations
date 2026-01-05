# mensa-localizations

Microservizio Go (Fiber) che fa da **proxy + cache** per i contenuti Tolgee:

- **Lingue** del progetto Tolgee
- **Export JSON** delle traduzioni (flat o nested)

La cache è **best-effort** e multilevel:

1. **Redis** (primary cache)
2. **S3/MinIO** (fallback `latest` + storico versionato)
3. **Tolgee** (fonte dati)

- **NEW**: l’aggiornamento ora è solo **on-demand** via webhook (`/api/:app/update`); non c’è più il refresh periodico ogni 15 minuti.

## API

Base URL: `http://localhost:3000`

### `GET /api/:app`
Ritorna la response Tolgee di:

- `https://app.tolgee.io/v2/projects/languages?ak=:app&size=1000`

Esempio:

```bash
curl -s "http://localhost:3000/api/<TOLGEE_AK>" | jq .
```

### `GET /api/:app/:lang`
Ritorna le traduzioni Tolgee tramite:

- `https://app.tolgee.io/v2/projects/export?ak=:app&languages=:lang&format=JSON&zip=false&size=1000`

Query params:
- `nested` (boolean, default `false`)
  - `false`: JSON “flat” (compat)
  - `true`: JSON “nested”

Esempi:

```bash
# flat
curl -s "http://localhost:3000/api/<TOLGEE_AK>/it" | jq .

# nested
curl -s "http://localhost:3000/api/<TOLGEE_AK>/it?nested=true" | jq .
```

### `ALL /api/:app/update`
Endpoint da chiamare via webhook per forzare il refresh di **lingue** e di **tutte le traduzioni** (flat + nested) per ogni lingua.

- Richiede query `?secret=<WEBHOOK_SECRET>` che deve combaciare con la variabile d’ambiente `WEBHOOK_SECRET`.
- Metodo accettato: qualunque (`GET`/`POST`/etc., lato webhook usare il preferito).
- Risposta: JSON con riepilogo di lingue trovate e tentativi di refresh.

Esempio:

```bash
curl -s "http://localhost:3000/api/<TOLGEE_AK>/update?secret=$WEBHOOK_SECRET" | jq .
```

### `GET /healthz`
Healthcheck semplice.

```bash
curl -s "http://localhost:3000/healthz"
```

## Caching

### Redis
Chiavi usate:
- Traduzioni: `translations:<app>:<lang>:<mode>`
- Lingue: `languages:<app>`
- Timestamp fetch (unix seconds): `<key>:fetched_utc`

TTL:
- Valore Redis: `10m` (costante `redisValueTTL` in `main/main.go`)

### S3 / MinIO
Quando S3 è abilitato, il servizio:
- legge un **fallback** da `latest.json`
- quando scarica da Tolgee, salva:
  - un oggetto **versionato** immutabile
  - e aggiorna `latest.json`

Percorsi (key) principali:

**Traduzioni**
- `localizations/<app>/<lang>_<mode>/latest.json`
- `localizations/<app>/<lang>_<mode>/<ts>_<sha>.json`

**Lingue**
- `tolgee-languages/<app>/latest.json`
- `tolgee-languages/<app>/<ts>_<sha>.json`

Metadata S3 (su oggetti scritti dal servizio):
- `created_utc` in formato `20060102T150405Z` (usato per capire staleness)
- `sha256`
- `app`, `lang` (solo per traduzioni)

## Variabili d’ambiente

Il progetto usa `github.com/caarlos0/env` (vedi `tools/env/init.go`).

### Redis
- `REDIS_ADDR` (default `localhost:6379`)
- `REDIS_PASSWORD` (default vuota)

### S3 / MinIO
- `S3_ENABLED` (default `true`)
- `S3_BUCKET` (**required** se `S3_ENABLED=true`)
- `S3_REGION` (default `us-east-1`)
- `S3_ENDPOINT` (**required** se `S3_ENABLED=true`)
- `S3_ACCESS_KEY` (**required** se `S3_ENABLED=true`)
- `S3_SECRET_KEY` (**required** se `S3_ENABLED=true`)
- `S3_FORCE_PATH_STYLE` (default `true`)

### Webhook
- `WEBHOOK_SECRET` (secret obbligatorio per chiamare `/api/:app/update`)

## Run (locale)

Prerequisiti: Go installato + Redis in esecuzione.

```bash
cd mensa-localizations

go run ./main
```

Il servizio ascolta su `:3000`.

## Run (Docker)

Build:

```bash
docker build -t mensa-localizations:local .
```

Run (esempio minimo con Redis esterno e S3 disabilitato):

```bash
docker run --rm -p 3000:3000 \
  -e REDIS_ADDR=host.docker.internal:6379 \
  -e S3_ENABLED=false \
  -e WEBHOOK_SECRET=mysecret \
  mensa-localizations:local
```

> Nota: su Linux al posto di `host.docker.internal` potrebbe servire l’IP del gateway Docker.

## Logging

Il servizio stampa log **molto verbosi** (prefisso `[cache]`) per capire facilmente:
- hit/miss/error Redis
- fallback S3 + metadata `created_utc`
- chiamate Tolgee
- richieste di refresh webhook e `singleflight`

## Note / gotchas

- L’AK Tolgee viene preso dal path parameter `:app`.
- In caso di problemi a Redis/S3/Tolgee, la risposta è best-effort e tende a restituire `{}`.
- Il refresh avviene **solo** quando chiamato l’endpoint `/api/:app/update` con secret valido (niente più refresh periodico).
