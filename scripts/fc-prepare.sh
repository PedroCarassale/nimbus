#!/usr/bin/env bash
# Builds the host files a Firecracker compute plane needs:
# the v1.18.0 binary, the pinned guest kernel, a static /sbin/nimbus-init,
# and one ext4 rootfs per runtime (docker export of the existing image).
# Idempotent: a rootfs is rebuilt when the runtime image id or nimbus-init changes.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FC_VERSION="v1.18.0"
KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/20261111-778a26c0594c-0/x86_64/vmlinux-6.1.188"
KERNEL_SHA="f5735774e2570d9df451fc5665f3525d3baf723fa6a8a413a03591d5742b39a5"
DEST="/var/lib/nimbus/fc"
ROOTFS_DIR="$DEST/rootfs"

if [[ "$(id -u)" -eq 0 ]]; then
  SUDO=""
else
  SUDO="sudo"
fi

log() { echo "[fc-prepare] $*"; }

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) FC_ARCH="x86_64" ;;
  *)
    echo "[fc-prepare] esta máquina es $arch; el kernel pineado es x86_64" >&2
    exit 1
    ;;
esac

if ! command -v docker >/dev/null; then
  echo "[fc-prepare] docker no está en el PATH" >&2
  exit 1
fi
if ! command -v mkfs.ext4 >/dev/null && [[ ! -x /usr/sbin/mkfs.ext4 ]]; then
  log "instalando e2fsprogs"
  $SUDO apt-get update -qq
  $SUDO apt-get install -y e2fsprogs
fi
if ! command -v go >/dev/null; then
  echo "[fc-prepare] go no está en el PATH" >&2
  exit 1
fi

install_binaries() {
  local current=""
  if [[ -x /usr/local/bin/firecracker ]]; then
    current="$(/usr/local/bin/firecracker --version 2>&1 | awk '/Firecracker/{print $2; exit}' || true)"
  fi
  if [[ "$current" == "$FC_VERSION" && -x /usr/local/bin/jailer ]]; then
    log "firecracker $FC_VERSION ya está en /usr/local/bin"
    return
  fi
  local tmp url fc jail
  tmp="$(mktemp -d)"
  url="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${FC_ARCH}.tgz"
  log "descargando $url"
  curl -fsSL "$url" -o "$tmp/fc.tgz"
  tar -xzf "$tmp/fc.tgz" -C "$tmp"
  fc="$(find "$tmp" -type f -name "firecracker-${FC_VERSION}-${FC_ARCH}" -print -quit)"
  jail="$(find "$tmp" -type f -name "jailer-${FC_VERSION}-${FC_ARCH}" -print -quit)"
  if [[ -z "$fc" || -z "$jail" ]]; then
    echo "[fc-prepare] el tarball no trae firecracker y jailer" >&2
    exit 1
  fi
  $SUDO install -m 0755 "$fc" /usr/local/bin/firecracker
  $SUDO install -m 0755 "$jail" /usr/local/bin/jailer
  rm -rf "$tmp"
  log "instalado $(/usr/local/bin/firecracker --version)"
}

install_kernel() {
  $SUDO mkdir -p "$DEST"
  $SUDO chown "$(id -u):$(id -g)" "$DEST"
  if [[ -f "$DEST/vmlinux" ]]; then
    local got
    got="$(sha256sum "$DEST/vmlinux" | awk '{print $1}')"
    if [[ "$got" == "$KERNEL_SHA" ]]; then
      log "kernel ya coincide ($KERNEL_SHA)"
      return
    fi
  fi
  log "descargando kernel"
  curl -fsSL "$KERNEL_URL" -o "$DEST/vmlinux.partial"
  local got
  got="$(sha256sum "$DEST/vmlinux.partial" | awk '{print $1}')"
  if [[ "$got" != "$KERNEL_SHA" ]]; then
    echo "[fc-prepare] sha256 del kernel: $got (se esperaba $KERNEL_SHA)" >&2
    exit 1
  fi
  mv "$DEST/vmlinux.partial" "$DEST/vmlinux"
  chmod 644 "$DEST/vmlinux"
  log "kernel en $DEST/vmlinux"
}

build_init() {
  local out="$DEST/nimbus-init"
  log "compilando nimbus-init"
  (
    cd "$ROOT/compute-plane"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "$out" ./cmd/fc-init
  )
  chmod 755 "$out"
  if ldd "$out" 2>&1 | grep -q '=>'; then
    echo "[fc-prepare] nimbus-init no salió estático" >&2
    exit 1
  fi
}

build_runtime_images() {
  log "imágenes de runtime"
  docker build -t nimbus-node "$ROOT/runtime-node"
  docker build -t nimbus-python "$ROOT/runtime-python"
  docker build -t nimbus-go "$ROOT/runtime-go"
}

build_rootfs() {
  local image="$1" name="$2"
  local id stamp dest stage cid bytes size
  id="$(docker image inspect -f '{{.Id}}' "$image")"
  init_sha="$(sha256sum "$DEST/nimbus-init" | awk '{print $1}')"
  want="${id} ${init_sha}"
  dest="$ROOTFS_DIR/${name}.ext4"
  stamp="$ROOTFS_DIR/${name}.stamp"
  mkdir -p "$ROOTFS_DIR"
  if [[ -f "$dest" && -f "$stamp" && "$(cat "$stamp")" == "$want" ]]; then
    log "rootfs $name al día ($id)"
    return
  fi
  log "armando rootfs $name desde $image"
  stage="$(mktemp -d)"
  cid="$(docker create "$image")"
  docker export "$cid" | tar -C "$stage" -xf -
  docker rm "$cid" >/dev/null
  mkdir -p "$stage/proc" "$stage/sys" "$stage/dev" "$stage/tmp" "$stage/run" \
    "$stage/mnt/code" "$stage/var/task" "$stage/sbin"
  install -m 0755 "$DEST/nimbus-init" "$stage/sbin/nimbus-init"
  bytes="$(du -sb "$stage" | awk '{print $1}')"
  size=$(( (bytes + 96 * 1024 * 1024 + 32 * 1024 * 1024 - 1) / (32 * 1024 * 1024) * 32 * 1024 * 1024 ))
  if (( size < 256 * 1024 * 1024 )); then
    size=$((256 * 1024 * 1024))
  fi
  rm -f "$dest"
  truncate -s "$size" "$dest"
  mkfs.ext4 -F -i 4096 -L "nimbus-$name" -d "$stage" "$dest"
  chmod 644 "$dest"
  echo "$want" > "$stamp"
  rm -rf "$stage"
  log "rootfs $dest ($((size / 1024 / 1024)) MiB)"
}

install_binaries
install_kernel
build_init
build_runtime_images
build_rootfs nimbus-node nodejs20
build_rootfs nimbus-python python3.12
build_rootfs nimbus-go go1.x

if [[ -e /dev/kvm ]]; then
  $SUDO chmod 666 /dev/kvm || true
  log "/dev/kvm modo $(stat -c '%A' /dev/kvm)"
else
  echo "[fc-prepare] no existe /dev/kvm" >&2
  exit 1
fi

log "listo"
