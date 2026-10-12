#!/usr/bin/env bash
# Runs Ollaya on this machine's NVIDIA GPU behind a private HTTPS tunnel, and prints a pairing
# code to paste into Zex (Connections, Cloud GPU).
#
#   curl -fsSL https://raw.githubusercontent.com/mahdiarfrm/zex/main/cloud/zex-gpu.sh | bash
#   ... | bash -s -- winnow:e4b    download this model instead of the one picked for the GPU
#   ... | bash -s -- none          download no model; pick one later in Zex, under Models
#   ... | bash -s -- stop          stop Ollaya and the tunnel
#
# It runs on Linux (x86_64 or arm64), on a GPU server or in a Colab or Kaggle notebook (run it
# from a cell, starting with !). Everything goes in ~/.zex-gpu; nothing is installed for the
# whole system. Ollaya's installer needs glibc 2.38 or newer. On older systems, such as the
# Ubuntu 22.04 that Colab and Kaggle run, the script uses the files of Ollaya's Docker image
# instead, with the C library that comes in the image.
#
# Settings: OLLAYA_VERSION (default 0.12.1), ZEX_GPU_DIR (default ~/.zex-gpu), ZEX_GPU_PORT
# (default 11435).
set -eu

VERSION=${OLLAYA_VERSION:-0.12.1}
DIR=${ZEX_GPU_DIR:-$HOME/.zex-gpu}
PORT=${ZEX_GPU_PORT:-11435}
CLOUDFLARED_VERSION=2025.8.1
PATCHELF_VERSION=0.18.0

say() { printf '\033[1mzex-gpu:\033[0m %s\n' "$*"; }
die() { printf '\033[1;31mzex-gpu:\033[0m %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }
alive() { [ -f "$1" ] && kill -0 "$(cat "$1")" 2>/dev/null; }
fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
up() { case $(curl -fsS -m 3 "http://127.0.0.1:$PORT/" 2>/dev/null || true) in *"Ollaya is running"*) return 0 ;; esac; return 1; }
tunnel_url() { grep -o 'https://[a-z0-9-]*\.trycloudflare\.com' "$DIR/tunnel.log" 2>/dev/null | sed -n 1p; }

stop_all() {
    for p in tunnel ollaya; do
        if alive "$DIR/$p.pid"; then kill "$(cat "$DIR/$p.pid")" 2>/dev/null || true; fi
        rm -f "$DIR/$p.pid"
    done
    say "stopped"
}

# starts a program in the background, detached from this shell, so it outlives a notebook cell
spawn() {
    name=$1; shift
    if have setsid; then setsid nohup "$@" >"$DIR/$name.log" 2>&1 </dev/null &
    else nohup "$@" >"$DIR/$name.log" 2>&1 </dev/null & fi
    echo $! >"$DIR/$name.pid"
}

ARG=${1:-auto}
mkdir -p "$DIR"
if [ "$ARG" = stop ]; then stop_all; exit 0; fi
[ "$(uname -s)" = Linux ] || die "this script runs on Linux; on a Mac, install Ollaya from ollaya.dev"
case $(uname -m) in
    x86_64 | amd64) ARCH=amd64; LOADER=ld-linux-x86-64.so.2; TRIPLE=x86_64-linux-gnu ;;
    aarch64 | arm64) ARCH=arm64; LOADER=ld-linux-aarch64.so.1; TRIPLE=aarch64-linux-gnu ;;
    *) die "unsupported architecture: $(uname -m)" ;;
esac
have curl || die "curl is needed"

# ---- where this runs, and its GPU
if [ -n "${COLAB_RELEASE_TAG:-}${COLAB_GPU:-}" ]; then WHERE=Colab
elif [ -n "${KAGGLE_KERNEL_RUN_TYPE:-}" ]; then WHERE=Kaggle
elif [ -n "${RUNPOD_POD_ID:-}" ]; then WHERE=RunPod
else WHERE=$(hostname 2>/dev/null || echo Linux); fi
GPU=; GPU_MIB=0; CUDA_DRIVER=0; CAP=0
if have nvidia-smi && nvidia-smi >/dev/null 2>&1; then
    GPU=$(nvidia-smi --query-gpu=name --format=csv,noheader | sed -n '1s/^ *//; 1s/ *$//; 1p')
    GPU_MIB=$(nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits | tr -dc '0-9\n' | sort -n | sed -n '$p')
    CUDA_DRIVER=$(nvidia-smi | sed -n 's/.*CUDA[A-Z ]*Version: *\([0-9]*\).*/\1/p' | sed -n 1p)
    CAP=$(nvidia-smi --query-gpu=compute_cap --format=csv,noheader | tr -dc '0-9\n' | sort -n | sed -n 1p)
    say "GPU: $GPU, $((${GPU_MIB:-0} / 1024)) GB"
else
    say "no NVIDIA GPU found: Ollaya will run on the CPU (in Colab: Runtime, Change runtime type, pick a GPU)"
fi
# the CUDA 13 pack needs a driver for CUDA 13 and a Turing or newer card; otherwise the CUDA 12 one
PACK=
if [ -n "$GPU" ]; then
    PACK=cuda
    if [ "${CUDA_DRIVER:-0}" -lt 13 ] || [ "${CAP:-0}" -lt 75 ]; then PACK=cuda12; fi
fi

# ---- a key for this server: Ollaya refuses requests without it
[ -s "$DIR/key" ] || head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$DIR/key"
KEY=$(cat "$DIR/key")
export OLLAYA_HOST="127.0.0.1:$PORT" OLLAYA_API_KEY="$KEY" OLLAYA_MODELS="$DIR/models"
if [ -z "$GPU" ]; then export OLLAYA_DEVICE=cpu; fi

# ---- Ollaya
glibc=$(getconf GNU_LIBC_VERSION 2>/dev/null | awk '{ print $2 }') || glibc=
LIBS=
if [ -n "$glibc" ] && printf '%s\n' "$glibc" | awk -F. '{ exit !($1 > 2 || ($1 == 2 && $2 >= 38)) }'; then
    OLLAYA=$DIR/ollaya/bin/ollaya
    if [ ! -x "$OLLAYA" ]; then
        say "installing Ollaya $VERSION"
        # the installer unpacks with zstd; a fresh server or notebook may not have it
        if ! have zstd && have apt-get && [ "$(id -u)" = 0 ]; then
            apt-get install -y -qq zstd >/dev/null 2>&1 || { apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq zstd >/dev/null 2>&1; } || true
        fi
        fetch https://ollaya.dev/install.sh "$DIR/install.sh"
        if [ -z "$GPU" ]; then export OLLAYA_NO_CUDA=1; fi
        OLLAYA_VERSION=$VERSION OLLAYA_INSTALL_DIR=$DIR/ollaya OLLAYA_NO_SERVICE=1 sh "$DIR/install.sh"
    fi
else
    # Ollaya's Docker image, unpacked: its own C library runs Ollaya on an older system
    TAG=$VERSION${PACK:+-$PACK}
    ROOT=$DIR/image-$TAG
    OLLAYA=$ROOT/usr/bin/ollaya
    if [ ! -f "$ROOT/.done" ]; then
        have python3 || die "python3 is needed to fetch Ollaya's image on this system (glibc ${glibc:-unknown})"
        say "this system has glibc ${glibc:-unknown}, older than Ollaya's installer needs: using Ollaya's Docker image ($TAG) instead"
        rm -rf "$ROOT"; mkdir -p "$ROOT"
        python3 - "$TAG" "$ARCH" "$ROOT" <<'PY'
import json, os, sys, tarfile, urllib.request
tag, arch, root = sys.argv[1:]
repo = "ollaya-dev/ollaya"
tok = json.load(urllib.request.urlopen(f"https://ghcr.io/token?scope=repository:{repo}:pull"))["token"]
def get(path, accept="*/*"):
    return urllib.request.urlopen(urllib.request.Request(f"https://ghcr.io/v2/{repo}/{path}", headers={"Authorization": "Bearer " + tok, "Accept": accept}))
KINDS = ("application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,"
         "application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json")
man = json.load(get(f"manifests/{tag}", KINDS))
if "manifests" in man:
    pick = [m for m in man["manifests"] if m.get("platform", {}).get("architecture") == arch]
    if not pick:
        sys.exit(f"Ollaya's image {tag} has no {arch} build")
    man = json.load(get(f"manifests/{pick[0]['digest']}", KINDS))
total, done = sum(l["size"] for l in man["layers"]), 0
for layer in man["layers"]:
    with get(f"blobs/{layer['digest']}") as r, tarfile.open(fileobj=r, mode="r|*") as t:
        for m in t:
            if os.path.basename(m.name).startswith(".wh.") or m.isdev():
                continue
            m.uid = m.gid = os.getuid()
            if sys.version_info >= (3, 12):
                t.extract(m, root, filter="fully_trusted")
            else:
                t.extract(m, root)
    done += layer["size"]
    print(f"zex-gpu: image {done * 100 // total}% of {total // 1_000_000} MB", flush=True)
PY
        # Ollaya's programs ask for the system's loader; point them at the image's, which then
        # loads the image's C library
        PATCHELF=$DIR/patchelf/bin/patchelf
        if [ ! -x "$PATCHELF" ]; then
            mkdir -p "$DIR/patchelf"
            fetch "https://github.com/NixOS/patchelf/releases/download/$PATCHELF_VERSION/patchelf-$PATCHELF_VERSION-$(uname -m).tar.gz" "$DIR/patchelf.tgz"
            tar -xzf "$DIR/patchelf.tgz" -C "$DIR/patchelf"; rm -f "$DIR/patchelf.tgz"
        fi
        ld=
        for c in "$ROOT/usr/lib/$TRIPLE/$LOADER" "$ROOT/lib/$TRIPLE/$LOADER" "$ROOT/usr/lib64/$LOADER"; do
            if [ -e "$c" ]; then ld=$c; break; fi
        done
        [ -n "$ld" ] || die "the image has no $LOADER"
        for f in $(find "$ROOT/usr/bin" "$ROOT/usr/lib/ollaya" -type f -perm -u+x 2>/dev/null || true); do
            if "$PATCHELF" --print-interpreter "$f" >/dev/null 2>&1; then "$PATCHELF" --set-interpreter "$ld" "$f"; fi
        done
        touch "$ROOT/.done"
    fi
    LIBS=$ROOT/usr/lib/$TRIPLE:$ROOT/lib/$TRIPLE
    # the GPU driver is this machine's: link only its libraries, so the system's older C library
    # stays out of the way
    if [ -n "$GPU" ]; then
        mkdir -p "$DIR/driver"
        for d in /usr/lib64-nvidia /usr/local/nvidia/lib64 "/usr/lib/$TRIPLE" /usr/lib64 /usr/lib; do
            for f in "$d"/libcuda.so* "$d"/libnvidia-*.so* "$d"/libnvcuvid.so*; do
                if [ -e "$f" ]; then ln -sf "$f" "$DIR/driver/"; fi
            done
        done
        LIBS=$LIBS:$DIR/driver
    fi
fi
[ -x "$OLLAYA" ] || die "Ollaya didn't install; see the messages above"
# Ollaya, with the image's libraries when it uses them (only Ollaya: this system's own programs
# would break on them)
ollaya() { if [ -n "$LIBS" ]; then env LD_LIBRARY_PATH="$LIBS" "$OLLAYA" "$@"; else "$OLLAYA" "$@"; fi; }

if ! alive "$DIR/ollaya.pid"; then
    say "starting Ollaya"
    if [ -n "$LIBS" ]; then spawn ollaya env LD_LIBRARY_PATH="$LIBS" "$OLLAYA" serve; else spawn ollaya "$OLLAYA" serve; fi
fi
for _ in $(seq 1 60); do if up; then break; fi; sleep 1; done
up || { tail -n 30 "$DIR/ollaya.log" >&2; die "Ollaya didn't start; its log is above"; }

# ---- a model: the recommended one when the GPU holds it, the small laya otherwise
MODEL=$ARG
if [ "$MODEL" = auto ]; then
    if [ -n "$PACK" ] && [ "${GPU_MIB:-0}" -ge 10240 ]; then MODEL=winnow:e4b; else MODEL=laya; fi
fi
if [ "$MODEL" != none ]; then
    say "downloading $MODEL (Zex lists more under Models)"
    ollaya pull "$MODEL"
fi

# ---- the tunnel: a private https address for this port, from Cloudflare, without an account
CF=$DIR/cloudflared
if [ ! -x "$CF" ]; then
    fetch "https://github.com/cloudflare/cloudflared/releases/download/$CLOUDFLARED_VERSION/cloudflared-linux-$ARCH" "$CF"
    chmod +x "$CF"
fi
URL=
if alive "$DIR/tunnel.pid"; then URL=$(tunnel_url); fi
if [ -z "$URL" ]; then
    say "opening the tunnel"
    # Ollaya answers only requests addressed to itself, so the tunnel says it is talking to 127.0.0.1
    spawn tunnel "$CF" tunnel --no-autoupdate --url "http://127.0.0.1:$PORT" --http-host-header "127.0.0.1:$PORT"
    for _ in $(seq 1 60); do URL=$(tunnel_url); if [ -n "$URL" ]; then break; fi; sleep 1; done
    [ -n "$URL" ] || { tail -n 20 "$DIR/tunnel.log" >&2; die "the tunnel didn't open; its log is above"; }
fi
# a new address takes a few seconds to resolve everywhere
for _ in $(seq 1 30); do
    if curl -fsS -m 5 -H "Authorization: Bearer $KEY" "$URL/v1/models" >/dev/null 2>&1; then break; fi
    sleep 2
done

# ---- the pairing code: the address and the key, for Zex
CODE="zex-gpu:$(printf '{"v":1,"url":"%s","key":"%s","where":"%s","gpu":"%s","mem":%s}' "$URL" "$KEY" "$WHERE" "$GPU" "${GPU_MIB:-0}" | base64 | tr -d '\n=' | tr '+/' '-_')"
printf '\n  Paste this into Zex, under Connections, Cloud GPU:\n\n  %s\n\n' "$CODE"
printf '  It works while this machine runs. Keep it private: it holds the key to this server.\n'
printf '  Run the script again to see it again; run it with "stop" to stop.\n\n'
