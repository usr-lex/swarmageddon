# shellcheck shell=sh
# shellcheck disable=SC1091
# nvm in interactive shells (`nvm use 22`, `nvm ls`, ...). PATH already points
# at the image's default Node through /opt/nvm/current.
export NVM_DIR=/opt/nvm
[ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh"
[ -n "${BASH_VERSION:-}" ] && [ -s "$NVM_DIR/bash_completion" ] && . "$NVM_DIR/bash_completion"
