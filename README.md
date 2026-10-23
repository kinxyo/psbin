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

Open the printed URL, or scan the QR code. Pastes go to `pastes.jsonl` in the current
directory.

If other devices can't connect, your firewall is probably blocking the port. On ufw:

```sh
sudo ufw allow from 192.168.1.0/24 to any port 8000 proto tcp
```

Windows and macOS ask to allow incoming connections the first time you run it.

## Flags

| Flag    | Env            | Default        | |
|---------|----------------|----------------|-|
| `-addr` | `ADDR`         | `:8000`        | Listen address. `9000` and `:9000` both work. |
| `-db`   | `DATABASE_URL` | empty          | Postgres connection string. If set, pastes go to Postgres. |
| `-data` |                | `pastes.jsonl` | Paste file, used when `-db` is empty. |

A flag overrides its environment variable.

## Storage

- **File (default):** one JSON object per line, appended on every paste and loaded into
  memory at startup. Fine for personal use; everything lives in RAM.
- **Postgres:** set `-db` or `DATABASE_URL`, e.g. `host=/run/postgresql dbname=psbin`.
  The `pastes` table is created if it doesn't exist. If Postgres can't be reached,
  psbin exits instead of falling back to the file, so pastes don't end up split across
  two places.

Switching between the two doesn't migrate existing pastes.

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
