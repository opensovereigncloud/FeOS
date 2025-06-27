#!/bin/bash
set -e

echo "Building UKI with objcopy..."

# Calculate section alignment from systemd stub
echo "Calculating section offsets..."
align="$(objdump -p /usr/lib/systemd/boot/efi/linuxx64.efi.stub | awk '/SectionAlignment/ {print $2}')"
align_dec=$((16#$align))
echo "Section alignment: $align ($align_dec bytes)"

# Calculate the end of the systemd stub to determine where our sections can start
stub_size="$(objdump -h /usr/lib/systemd/boot/efi/linuxx64.efi.stub | tail -n +2 | awk '{if(NF==7) {size=$3; offset=$4; printf "%d\n", ("0x" size) + ("0x" offset)}}' | sort -n | tail -1)"

# Calculate properly aligned offsets for each section
osrel_offs=$((stub_size + align_dec - stub_size % align_dec))
cmdline_offs=$((osrel_offs + $(stat -Lc%s "/usr/lib/os-release")))
cmdline_offs=$((cmdline_offs + align_dec - cmdline_offs % align_dec))
initrd_offs=$((cmdline_offs + $(stat -Lc%s "/feos/target/cmdline")))
initrd_offs=$((initrd_offs + align_dec - initrd_offs % align_dec))
linux_offs=$((initrd_offs + $(stat -Lc%s "/feos/target/initramfs.zst")))
linux_offs=$((linux_offs + align_dec - linux_offs % align_dec))

echo "Calculated offsets:"
echo "  .osrel:   0x$(printf %x $osrel_offs)"
echo "  .cmdline: 0x$(printf %x $cmdline_offs)"
echo "  .initrd:  0x$(printf %x $initrd_offs)" 
echo "  .linux:   0x$(printf %x $linux_offs)"

# Create UKI using objcopy with calculated offsets
echo "Creating UKI with objcopy..."
objcopy \
    --add-section .osrel=/feos/hack/uki/os-release.txt \
    --change-section-vma .osrel=$(printf 0x%x $osrel_offs) \
    --add-section .cmdline=/feos/target/cmdline \
    --change-section-vma .cmdline=$(printf 0x%x $cmdline_offs) \
    --add-section .initrd=/feos/target/initramfs.zst \
    --change-section-vma .initrd=$(printf 0x%x $initrd_offs) \
    --add-section .linux=/feos/target/kernel/vmlinuz \
    --change-section-vma .linux=$(printf 0x%x $linux_offs) \
    /usr/lib/systemd/boot/efi/linuxx64.efi.stub /feos/target/uki.efi

# Sign the UKI with secureboot key
echo "Signing UKI with secureboot key..."
sbsign \
    --key /feos/keys/secureboot.key \
    --cert /feos/keys/secureboot.pem \
    --output /feos/target/uki.efi \
    /feos/target/uki.efi

echo "UKI created successfully at target/uki.efi" 