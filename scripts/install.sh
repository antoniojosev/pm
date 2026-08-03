#!/usr/bin/env bash
# Instala pm: compila el binario y lo deja en ~/.local/bin (o $GOBIN).
set -euo pipefail
cd "$(dirname "$0")/.."

DEST="${1:-$HOME/.local/bin}"
mkdir -p "$DEST"

echo "▶ compilando pm…"
go build -o "$DEST/pm" ./cmd/pm
echo "✓ pm instalado en $DEST/pm"

case ":$PATH:" in
  *":$DEST:"*) ;;
  *) echo "⚠ añade $DEST a tu PATH:  echo 'export PATH=\"$DEST:\$PATH\"' >> ~/.zshrc" ;;
esac

echo
echo "Siguiente:"
echo "  pm setup     # prepara ~/.pm, el proxy y las units de systemd"
echo "  pm doctor    # verifica el entorno"
echo "  pm ps        # ve qué corre ahora"
