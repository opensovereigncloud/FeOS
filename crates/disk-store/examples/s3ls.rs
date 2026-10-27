// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

#[tokio::main]
async fn main() {
    let prefix = std::env::args().nth(1).unwrap_or_default();
    let s3 = disk_store::s3::S3::from_env(prefix)
        .unwrap()
        .expect("S3 creds");
    use futures::stream::StreamExt;
    use object_store::ObjectStore;
    let store = s3.raw_store();
    let mut stream = store.list(None);
    let mut n = 0u64;
    while let Some(item) = stream.next().await {
        let m = item.unwrap();
        println!("{} {}", m.location, m.size);
        n += 1;
        if n > 4000 {
            println!("... truncated");
            break;
        }
    }
    eprintln!("{n} objects");
}
