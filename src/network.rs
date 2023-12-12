use futures::stream::TryStreamExt;
use log::info;
use rtnetlink::new_connection;

pub async fn configure_network_devices() -> Result<(), String> {
    // TODO: configure network devices

    let (connection, handle, _) = new_connection().unwrap();
    tokio::spawn(connection);

    let mut link_ts = handle
        .link()
        .get()
        .match_name(String::from("eth0"))
        .execute();

    let link = link_ts
        .try_next()
        .await
        .map_err(|e| format!("'eth0' not found: {}", e))?
        .ok_or("option A empty".to_string())?;

    info!("eth0:");
    for attr in link.attributes {
        match attr {
            netlink_packet_route::link::LinkAttribute::Address(mac_bytes) => {
                info!("  mac: {}", format_mac(mac_bytes));
            }
            netlink_packet_route::link::LinkAttribute::Carrier(carrier) => {
                info!("  carrier: {}", carrier);
            }
            netlink_packet_route::link::LinkAttribute::Mtu(mtu) => {
                info!("  mtu: {}", mtu);
            }
            netlink_packet_route::link::LinkAttribute::MaxMtu(max_mtu) => {
                info!("  max_mtu: {}", max_mtu);
            }
            netlink_packet_route::link::LinkAttribute::OperState(state) => {
                let state = match state {
                    netlink_packet_route::link::State::Unknown => String::from("unknown"),
                    netlink_packet_route::link::State::NotPresent => String::from("not present"),
                    netlink_packet_route::link::State::Down => String::from("down"),
                    netlink_packet_route::link::State::LowerLayerDown => {
                        String::from("lower layer down")
                    }
                    netlink_packet_route::link::State::Testing => String::from("testing"),
                    netlink_packet_route::link::State::Dormant => String::from("dormant"),
                    netlink_packet_route::link::State::Up => String::from("up"),
                    netlink_packet_route::link::State::Other(x) => {
                        format!("other ({})", x)
                    }
                    _ => String::from("unknown state"),
                };
                info!("  state: {}", state);
            }
            _ => (),
        }
    }

    let mut addr_ts = handle
        .address()
        .get()
        .set_link_index_filter(link.header.index)
        .execute();

    while let Some(addr_msg) = addr_ts
        .try_next()
        .await
        .map_err(|e| format!("Could not get addr: {}", e))?
    {
        for attr in addr_msg.attributes {
            if let netlink_packet_route::address::AddressAttribute::Address(addr) = attr {
                info!("- {}/{}", addr, addr_msg.header.prefix_len);
            }
        }
    }

    Ok(())
}

fn format_mac(bytes: Vec<u8>) -> String {
    bytes
        .iter()
        .map(|byte| format!("{:02x}", byte))
        .collect::<Vec<String>>()
        .join(":")
}
