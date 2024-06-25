2024-06-25 How to bind PCIe VFs to cloud-hypervisor VMs
===

### Preconditions

Bind modules
```
modprobe vfio_iommu_type1 allow_unsafe_interrupts
modprobe vfio_pci
```

### Add pci device
The following steps are needed in order to use a device with the following pci address `0000:06:00.2`

Unbind if bound
```
echo 0000:06:00.2 > /sys/bus/pci/devices/0000:06:00.2/driver/unbind
```

Bind to vfio
```
echo  {vendorID} {deviceID} > /sys/bus/pci/drivers/vfio-pci/new_id
# echo  15b3 101e > /sys/bus/pci/drivers/vfio-pci/new_id
```

Reference device in cloud-hypervisor
```
--device path=/sys/bus/pci/devices/0000:06:00.2/
```