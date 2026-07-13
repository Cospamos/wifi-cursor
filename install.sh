#!/usr/bin/env bash
# Устанавливает wifi-cursor: качает готовый бинарник из GitHub Releases
# и добавляет глобальный алиас, чтобы вызывать его из любой точки.
#
# Использование:
#   curl -fsSL https://raw.githubusercontent.com/Cospamos/wifi-cursor/master/install.sh | bash
#   ./install.sh v0.1.0     # конкретная версия вместо latest

set -euo pipefail

REPO="Cospamos/wifi-cursor"
ASSET="wifi-cursor-linux-amd64"
VERSION="${1:-latest}"
INSTALL_DIR="$HOME/.wifi-cursor/bin"
BIN_PATH="$INSTALL_DIR/wifi-cursor"

if [ "$VERSION" = "latest" ]; then
  URL="https://github.com/$REPO/releases/latest/download/$ASSET"
else
  URL="https://github.com/$REPO/releases/download/$VERSION/$ASSET"
fi

echo "=== wifi-cursor installer ==="
mkdir -p "$INSTALL_DIR"

echo "Скачиваю $URL"
if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$URL" -o "$BIN_PATH"
elif command -v wget >/dev/null 2>&1; then
  wget -q "$URL" -O "$BIN_PATH"
else
  echo "Нужен curl или wget." >&2
  exit 1
fi
chmod +x "$BIN_PATH"

echo "Установлено: $BIN_PATH"

detect_rc() {
  case "$(basename "${SHELL:-bash}")" in
    zsh) echo "$HOME/.zshrc" ;;
    bash) echo "$HOME/.bashrc" ;;
    *) echo "$HOME/.profile" ;;
  esac
}

RC_FILE="$(detect_rc)"
touch "$RC_FILE"

if ! grep -Fq "$BIN_PATH" "$RC_FILE" 2>/dev/null; then
  {
    echo ""
    echo "# wifi-cursor"
    echo "alias wifi-cursor=\"$BIN_PATH\""
  } >> "$RC_FILE"
  echo "Глобальный алиас добавлен в $RC_FILE"
else
  echo "Алиас уже есть в $RC_FILE"
fi

echo ""
echo "Готово! Откройте новый терминал или выполните:"
echo "  source $RC_FILE"
echo "После этого можно запускать: wifi-cursor create"
