#!/bin/bash

PORT=80

# `for arg in "$@"` snapshots the list before the body runs, so the shifts did
# nothing and "$2" was always the script's second argument wherever -port
# appeared. `./statusnook.sh --docker -port 8080` set PORT to the literal
# string "-port", which fails the numeric test below and writes an unstartable
# unit file.
while [ $# -gt 0 ]; do
    case "$1" in
        -port)
            if [ $# -lt 2 ]; then
                echo "-port requires a value" >&2
                exit 1
            fi
            PORT="$2"
            shift 2
            ;;
        *)
            shift
            ;;
    esac
done

case "$PORT" in
    ""|*[!0-9]*)
        echo "-port must be a number, got: $PORT" >&2
        exit 1
        ;;
esac


useradd statusnook --system -m

cd /home/statusnook


case $(uname -m) in
    x86_64)
        goarch="amd64"
        ;;
    aarch64)
        goarch="arm64"
        ;;
    *)
        echo "unknown arch"
        exit 1
        ;;
esac

curl -fsSL https://get.statusnook.com/statusnook_linux_${goarch}_v0.3.0 -o /home/statusnook/statusnook

chmod +x /home/statusnook/statusnook

cat <<EOF >/etc/systemd/system/statusnook.service
[Unit]
Description=Statusnook
After=network.target

[Service]
Type=simple
Restart=always
User=statusnook
WorkingDirectory=/home/statusnook
ExecStart=/home/statusnook/statusnook --port $PORT
EOF
if [ "$PORT" -eq 80 ]; then
    echo -e "AmbientCapabilities=CAP_NET_BIND_SERVICE\n" >> /etc/systemd/system/statusnook.service
else
    echo -e "" >> /etc/systemd/system/statusnook.service
fi
cat <<EOF >>/etc/systemd/system/statusnook.service
[Install]
WantedBy=multi-user.target
EOF

# Generated before the service starts, not after: the server would otherwise
# serve its first TLS requests with no fallback certificate on disk.
FINGERPRINT="$(su - statusnook -c "/home/statusnook/statusnook -generate-self-signed-cert")"

systemctl enable statusnook > /dev/null 2>&1
systemctl start statusnook

echo "Self-signed certificate SHA-256 fingerprint: $FINGERPRINT"

echo -e '\n\033[0;32mStatusnook successfully installed!\033[0m'
echo "To finalize your Statusnook instance setup, navigate to https://<your-ip-address-or-domain> in a web browser"
