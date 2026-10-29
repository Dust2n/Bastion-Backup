#!/bin/bash
# Erstellt das Bastion-Rettungssystem für Linux: Kernel + Initramfs (komplett
# im RAM, auch per PXE bootbar) und optional eine ISO für USB-Sticks.
#
# Voraussetzungen (Debian/Ubuntu): debootstrap cpio zstd
# Für die ISO zusätzlich: grub-pc-bin grub-efi-amd64-bin xorriso mtools
#
# Beispiel:
#   sudo ./build.sh --server https://backup01:8443 --fingerprint 7cca21... \
#        --agent ../../dist/linux-amd64/bastion-agent --out /var/lib/bastion-server/rescue
set -euo pipefail

SERVER="" FP="" AGENT="" OUT="./out" SUITE="bookworm" MIRROR="http://deb.debian.org/debian" ISO=1
while [ $# -gt 0 ]; do
  case "$1" in
    --server) SERVER="$2"; shift 2 ;;
    --fingerprint) FP="$2"; shift 2 ;;
    --agent) AGENT="$2"; shift 2 ;;
    --out) OUT="$2"; shift 2 ;;
    --suite) SUITE="$2"; shift 2 ;;
    --mirror) MIRROR="$2"; shift 2 ;;
    --no-iso) ISO=0; shift ;;
    *) echo "unbekannte Option $1"; exit 2 ;;
  esac
done
[ "$(id -u)" = 0 ] || { echo "Bitte als root ausführen"; exit 1; }
[ -n "$AGENT" ] && [ -f "$AGENT" ] || { echo "--agent fehlt"; exit 1; }
for t in debootstrap cpio zstd; do command -v $t >/dev/null || { echo "$t fehlt (apt install $t)"; exit 1; }; done

WORK=$(mktemp -d /tmp/bastion-rescue.XXXX)
ROOT="$WORK/root"
trap 'umount -l "$ROOT/proc" 2>/dev/null || true; rm -rf "$WORK"' EXIT
mkdir -p "$OUT"

echo "== Basissystem ($SUITE)"
debootstrap --variant=minbase \
  --include=linux-image-amd64,udev,kmod,util-linux,fdisk,gdisk,parted,e2fsprogs,xfsprogs,btrfs-progs,dosfstools,lvm2,mdadm,isc-dhcp-client,iproute2,ca-certificates,efibootmgr,procps,psmisc,busybox,zstd,firmware-linux-free,kbd,console-setup \
  "$SUITE" "$ROOT" "$MIRROR"

echo "== Bastion einrichten"
install -D -m 755 "$AGENT" "$ROOT/opt/bastion/bastion-agent"
mkdir -p "$ROOT/etc/bastion"
if [ -n "$SERVER" ]; then
  printf '{"server":"%s","fingerprint":"%s"}\n' "$SERVER" "$FP" > "$ROOT/etc/bastion/rescue.json"
fi
echo "bastion-rescue" > "$ROOT/etc/hostname"
# deutsches Tastaturlayout
sed -i 's/^XKBLAYOUT=.*/XKBLAYOUT="de"/' "$ROOT/etc/default/keyboard" 2>/dev/null || true

cat > "$ROOT/init" <<'EOF'
#!/bin/sh
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
mount -t proc proc /proc
mount -t sysfs sys /sys
mount -t devtmpfs dev /dev
mkdir -p /dev/pts /run/lock /tmp
mount -t devpts devpts /dev/pts
mount -t tmpfs tmpfs /run
[ -d /sys/firmware/efi/efivars ] && mount -t efivarfs efivarfs /sys/firmware/efi/efivars
/lib/systemd/systemd-udevd --daemon >/dev/null 2>&1
udevadm trigger --action=add
udevadm settle --timeout=30
setupcon -k >/dev/null 2>&1 || loadkeys de >/dev/null 2>&1

echo "Bastion Rettungssystem – Netzwerk wird eingerichtet ..."
ip link set lo up
IFACES=$(ls /sys/class/net | grep -v '^lo$')
for i in $IFACES; do ip link set "$i" up; done
dhclient -1 -timeout 30 $IFACES >/dev/null 2>&1 || echo "Warnung: keine IP-Adresse per DHCP erhalten"

S=""; F=""; AUTO=""
for a in $(cat /proc/cmdline); do
  case "$a" in
    bastion.server=*) S="${a#*=}" ;;
    bastion.fp=*) F="${a#*=}" ;;
    bastion.auto=1) AUTO="--auto" ;;
  esac
done
[ -n "$S" ] && printf '{"server":"%s","fingerprint":"%s"}\n' "$S" "$F" > /etc/bastion/rescue.json

while true; do
  setsid -c /opt/bastion/bastion-agent rescue $AUTO </dev/tty1 >/dev/tty1 2>&1
  AUTO=""
  echo "Bastion beendet. Shell für Diagnose ('reboot -f' für Neustart)." >/dev/tty1
  setsid -c /bin/sh </dev/tty1 >/dev/tty1 2>&1
done
EOF
chmod 755 "$ROOT/init"

echo "== Verkleinern"
rm -rf "$ROOT"/usr/share/doc/* "$ROOT"/usr/share/man/* "$ROOT"/usr/share/info/* "$ROOT"/var/cache/apt/* "$ROOT"/var/lib/apt/lists/*
find "$ROOT/usr/share/locale" -mindepth 1 -maxdepth 1 ! -name 'de*' ! -name 'en*' -exec rm -rf {} + 2>/dev/null || true

KVER=$(ls "$ROOT/lib/modules" | head -n1)
cp "$ROOT/boot/vmlinuz-$KVER" "$OUT/linux-vmlinuz"
rm -rf "$ROOT/boot"/*

echo "== Initramfs packen"
(cd "$ROOT" && find . -xdev | cpio -o -H newc --quiet | zstd -T0 -15 -q) > "$OUT/linux-initrd.img"

if [ "$ISO" = 1 ] && command -v grub-mkrescue >/dev/null; then
  echo "== ISO"
  ISOD="$WORK/iso"
  mkdir -p "$ISOD/boot/grub"
  cp "$OUT/linux-vmlinuz" "$ISOD/boot/vmlinuz"
  cp "$OUT/linux-initrd.img" "$ISOD/boot/initrd.img"
  cat > "$ISOD/boot/grub/grub.cfg" <<EOF
set timeout=5
menuentry "Bastion Wiederherstellung" {
  linux /boot/vmlinuz quiet
  initrd /boot/initrd.img
}
EOF
  grub-mkrescue -o "$OUT/bastion-rescue-linux.iso" "$ISOD" >/dev/null 2>&1 || echo "ISO konnte nicht erstellt werden (grub-pc-bin, grub-efi-amd64-bin, xorriso, mtools installiert?)"
fi

ls -lh "$OUT"/linux-vmlinuz "$OUT"/linux-initrd.img "$OUT"/bastion-rescue-linux.iso 2>/dev/null || true
echo "Fertig. Hinweis: Das Rettungssystem läuft komplett im RAM und benötigt mindestens 2 GB Arbeitsspeicher."
