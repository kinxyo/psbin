# psbin

A tiny pastebin. Paste text in a browser, get a link. One binary, no setup.

On startup it prints the LAN URL and a QR code, so a phone on the same wifi can scan
it instead of typing the IP.

## Run

Download the binary for your OS from the [latest release](https://github.com/kinxyo/psbin/releases/latest),
then:

```sh
chmod +x psbin-linux-amd64   # not needed on Windows
./psbin-linux-amd64
```

Open the printed URL, or scan the QR code. There's nothing to configure: pastes and the
encryption key are created on first run in a fixed per-user folder (see
[Where data lives](#where-data-lives)).

If other devices can't connect, your firewall is probably blocking the port. On ufw:

```sh
sudo ufw allow from <your-lan-subnet> to any port 8000 proto tcp   # e.g. 10.0.0.0/24
```

Windows and macOS ask to allow incoming connections the first time you run it.

## Flags

| Flag    | Env            | Default        | |
|---------|----------------|----------------|-|
| `-addr` | `ADDR`         | `:8000`        | Listen address. `9000` and `:9000` both work. |
| `-db`   | `DATABASE_URL` | empty          | Postgres connection string. Optional; by default pastes go to a local file. |
| `-log`  | `LOG_FORMAT`   | `text`         | Log format: `text` or `json`. |

A flag overrides its environment variable.

## Logs

Logs go to stderr (let systemd/Docker collect them): one line per request (method,
path, status, bytes, duration, remote address), plus one per paste created (id and size
only; paste text is never logged). Use `-log json` in prod.

## CLI

The same binary can list and read pastes without a browser, a running server or SQL:

```sh
psbin ls [-n 20]     # table of every paste: id, time, start of the text
psbin cat <id>       # print one paste in full
psbin help           # usage
```

(`psbin` is whatever you named the binary, e.g. `./psbin-linux-amd64 ls`, or `make prod`
installs it as `~/bin/psbin`.) With no subcommand, psbin runs the server.

```
ID        CREATED       MESSAGE
1f24b580  28 Oct 01:30  hello world
```

Both read the store directly and decrypt with the same key as the server, so run them
as the same user. `ls` prints which store it read to stderr. The paste file is opened
read-only, so it's safe while the server runs.

## Where data lives

One fixed folder per user, no matter where you run psbin from: `$PSBIN_DIR` if set,
otherwise `psbin/` in your config dir (`~/.config/psbin` on Linux,
`~/Library/Application Support/psbin` on macOS, `%AppData%\psbin` on Windows).

| File          | |
|---------------|-|
| `pastes.jsonl` | One JSON object per line, appended on every paste and loaded into memory at startup. Fine for personal use. |
| `.env`        | `PSBIN_KEY=<64 hex chars>`, the encryption key. Created on first run, mode 0600. |

## Encryption

Paste text is encrypted with AES-256-GCM before it's stored, so `pastes.jsonl` (or a
database, or a backup of either) holds only ciphertext. Each paste is bound to its id, so
ciphertexts can't be swapped between rows. The id and timestamp are not encrypted.

- **Back up `.env`.** Without the key the pastes can't be read. If pastes exist but the
  key is missing, psbin refuses to start rather than make a new key that would orphan them.
  `PSBIN_KEY` in the environment overrides the file.
- **What this protects:** someone who gets the data file or database but not `.env`.
- **What it doesn't:** there's still no auth. Anyone who can reach the port can read
  every paste through the web UI, and anyone who can read your `.env` can decrypt the file.
- Pastes saved before encryption was added are still readable (they have no `enc1:`
  prefix) but stay plaintext on disk. Move an old `pastes.jsonl` into the folder above to keep
  them.
- **Postgres** is opt-in: set `-db` or `DATABASE_URL`. The key still comes from the folder
  above. The `pastes` table is created if missing, and if Postgres can't be reached psbin
  exits instead of falling back to the file. Switching stores doesn't migrate pastes.

## Routes

| Route                | |
|----------------------|-|
| `GET /`              | Paste form and the 20 most recent pastes |
| `POST /paste`        | Save a paste (form field `content`), redirects to its page |
| `GET /p/{id}`        | View a paste |
| `GET /p/{id}/raw`    | Plain text |

Pastes are limited to 1 MiB.

## No auth

Anyone who can reach the port can read and write pastes. Keep it on a network you trust.

## Build

```sh
go build -o psbin .
```

or `make run` / `make prod` (`prod` installs to `~/bin`).
