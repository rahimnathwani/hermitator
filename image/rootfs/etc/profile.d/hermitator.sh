# Tools provided by the host at runtime (read-only bind mount).
case ":$PATH:" in
  *:/opt/hermitator/bin:*) ;;
  *) PATH="/opt/hermitator/bin:$PATH" ;;
esac
export PATH
export DISABLE_AUTOUPDATER=1
export PUPPETEER_EXECUTABLE_PATH=/usr/local/bin/chromium
