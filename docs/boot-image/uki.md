Unified Kernel Images
=====================

Have a look at the [Arch Documentation - Unified Kernel Images](https://wiki.archlinux.org/title/Unified_kernel_image).
It also describes how to build a UKI with `objcopy` instead of `ukify`.

## Boot from USB Stick
Booting FeOS from USB is quite easy. You just need to prepare a USB stick with a bootable UEFI partition and insert the FeOS UKI.

> This will wipe all data on your USB stick!

1. Download the latest FeOS UKI from the [Releases page](https://github.com/maltej/feos/releases).
2. Insert a USB stick into your machine and identify its device address using `sudo parted -l` - for this example we assume the USB stick is `/dev/sda`
3. Create a new GPT partition table and an ESP partition: `sudo parted /dev/sda --script -- mklabel gpt mkpart primary fat32 1MiB 100% set 1 on`
4. Format the newly created partition with FAT32 `sudo mkfs.fat -F32 /dev/sda1`
5. Mount the partition `sudo mount /dev/sda1 /mnt`
6. Create the EFI directory structure `sudo mkdir -p /mnt/EFI/BOOT`
7. Copy the FeOS UKI to the EFI partition `sudo cp feos-*.efi /mnt/EFI/BOOT/BOOTX64.EFI`
8. Optional: copy the secureboot certificate also to the USB stick `sudo cp feos.crt /mnt/` - you can install it into the UEFI secureboot certificate DB of your machine via the system setup (aka BIOS).

Now you have a UEFI bootable USB stick with FeOS.