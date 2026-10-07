# Boots the packaged compass-server on a PATH with no secretspec. Boot resolves
# the master key through the secretspec CLI, so it answers WhoAmI only if the
# package carries its own CLI.
{ pkgs, compass-server }:
pkgs.runCommand "compass-server-secretspec"
  {
    nativeBuildInputs = [
      pkgs.postgresql
      pkgs.nats-server
      pkgs.curl
    ];
  }
  ''
    export HOME="$TMPDIR"
    if command -v secretspec >/dev/null; then
      echo "secretspec is on the check's PATH; the check would prove nothing" >&2
      exit 1
    fi

    initdb -D "$TMPDIR/pg" -U compass --auth=trust >/dev/null
    postgres -D "$TMPDIR/pg" -k "$TMPDIR" -c listen_addresses= >"$TMPDIR/pg.log" 2>&1 &
    nats-server -js -a 127.0.0.1 -p 4222 -m 8222 -sd "$TMPDIR/nats" >"$TMPDIR/nats.log" 2>&1 &
    for _ in $(seq 100); do
      pg_isready -q -t 1 -h "$TMPDIR" && curl -sf --max-time 1 http://127.0.0.1:8222/healthz >/dev/null && break
      sleep 0.2
    done
    pg_isready -q -t 1 -h "$TMPDIR" || { cat "$TMPDIR/pg.log" >&2; exit 1; }

    printf 'COMPASS_MASTER_KEY="%s"\n' \
      00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff >"$TMPDIR/key.env"
    mkdir -m 700 "$TMPDIR/run"
    ${compass-server}/bin/compass-server \
      --socket "$TMPDIR/run/compass.sock" \
      --database "postgres:///postgres?host=$TMPDIR&user=compass" \
      --nats-url nats://127.0.0.1:4222 \
      --state-dir "$TMPDIR/state" \
      --secret-provider "dotenv://$TMPDIR/key.env" >"$TMPDIR/server.log" 2>&1 &
    server=$!

    # The socket binds before boot resolves secrets; requests are served after.
    for _ in $(seq 120); do
      if curl -sf --max-time 1 --unix-socket "$TMPDIR/run/compass.sock" --http2-prior-knowledge \
        -H 'Content-Type: application/json' -d '{}' \
        http://compass/compass.v1.CompassService/WhoAmI >"$TMPDIR/whoami.json"; then
        cat "$TMPDIR/whoami.json"
        touch "$out"
        exit 0
      fi
      if ! kill -0 "$server" 2>/dev/null; then
        echo "compass-server exited during boot:" >&2
        cat "$TMPDIR/server.log" >&2
        exit 1
      fi
      sleep 0.5
    done
    echo "compass-server never answered WhoAmI:" >&2
    cat "$TMPDIR/server.log" >&2
    exit 1
  ''
