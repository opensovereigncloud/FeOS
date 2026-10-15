// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Security groups (ADR 0012): `feos.network.v1.SecurityGroupService`, the
//! groups kept on disk, and the compiler that turns them and the node's
//! pods into sets of addresses and rules for nftables.

mod group;
mod policy;
mod prefix;
mod ranges;
mod registry;
mod service;
mod store;

pub use group::{check_id, Protocol};
pub use policy::{
    compile, delta, AddressRange, AddressSet, Allow, Delta, Pod, PodPolicy, Policy, SetChange,
    DENY4, DENY6, INTERNET4, INTERNET6,
};
pub use prefix::{Family, Prefix};
pub use service::SecurityGroups;
pub use store::Store;

use feos_proto::network_service::security_group_service_server::SecurityGroupService;
use feos_proto::network_service::{
    DeleteSecurityGroupRequest, DeleteSecurityGroupResponse, GetSecurityGroupRequest,
    ListSecurityGroupsRequest, ListSecurityGroupsResponse, PutSecurityGroupRequest,
    PutSecurityGroupResponse, SecurityGroup, UpdateSecurityGroupRequest,
    UpdateSecurityGroupResponse,
};
use tonic::{Request, Response, Status};

/// Where the security groups are kept: a file each.
pub const SECURITY_GROUP_DIR: &str = "/var/lib/feos/security-groups";

#[tonic::async_trait]
impl SecurityGroupService for SecurityGroups {
    async fn put_security_group(
        &self,
        request: Request<PutSecurityGroupRequest>,
    ) -> Result<Response<PutSecurityGroupResponse>, Status> {
        let group = request
            .into_inner()
            .group
            .ok_or_else(|| Status::invalid_argument("no security group"))?;
        self.put(group)?;
        Ok(Response::new(PutSecurityGroupResponse {}))
    }

    async fn update_security_group(
        &self,
        request: Request<UpdateSecurityGroupRequest>,
    ) -> Result<Response<UpdateSecurityGroupResponse>, Status> {
        self.update(&request.into_inner())?;
        Ok(Response::new(UpdateSecurityGroupResponse {}))
    }

    async fn delete_security_group(
        &self,
        request: Request<DeleteSecurityGroupRequest>,
    ) -> Result<Response<DeleteSecurityGroupResponse>, Status> {
        self.delete(&request.into_inner().id)?;
        Ok(Response::new(DeleteSecurityGroupResponse {}))
    }

    async fn get_security_group(
        &self,
        request: Request<GetSecurityGroupRequest>,
    ) -> Result<Response<SecurityGroup>, Status> {
        Ok(Response::new(self.get(&request.into_inner().id)?))
    }

    async fn list_security_groups(
        &self,
        _request: Request<ListSecurityGroupsRequest>,
    ) -> Result<Response<ListSecurityGroupsResponse>, Status> {
        Ok(Response::new(ListSecurityGroupsResponse {
            groups: self.list(),
        }))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tonic::Code;

    #[tokio::test]
    async fn the_service_serves_the_groups() {
        let dir = tempfile::tempdir().unwrap();
        let groups = SecurityGroups::open(dir.path()).unwrap();
        let group = SecurityGroup {
            id: "web".to_string(),
            version: 1,
            rules: vec![],
            members: vec!["fd00::1".to_string()],
        };
        let put = PutSecurityGroupRequest { group: None };
        let error = groups
            .put_security_group(Request::new(put))
            .await
            .unwrap_err();
        assert_eq!(error.code(), Code::InvalidArgument);
        let put = PutSecurityGroupRequest {
            group: Some(group.clone()),
        };
        groups.put_security_group(Request::new(put)).await.unwrap();

        let update = UpdateSecurityGroupRequest {
            id: "web".to_string(),
            from_version: 1,
            version: 2,
            remove_members: vec!["fd00::1".to_string()],
            ..Default::default()
        };
        groups
            .update_security_group(Request::new(update))
            .await
            .unwrap();
        let get = GetSecurityGroupRequest {
            id: "web".to_string(),
        };
        let got = groups.get_security_group(Request::new(get)).await.unwrap();
        assert_eq!((got.get_ref().version, got.get_ref().members.len()), (2, 0));

        let listed = groups
            .list_security_groups(Request::new(ListSecurityGroupsRequest {}))
            .await
            .unwrap();
        assert_eq!(listed.get_ref().groups.len(), 1);
        groups.set_in_use(|id| id == "web");
        let delete = || {
            Request::new(DeleteSecurityGroupRequest {
                id: "web".to_string(),
            })
        };
        let error = groups.delete_security_group(delete()).await.unwrap_err();
        assert_eq!(error.code(), Code::FailedPrecondition);
        groups.set_in_use(|_| false);
        groups.delete_security_group(delete()).await.unwrap();
        let error = groups.delete_security_group(delete()).await.unwrap_err();
        assert_eq!(error.code(), Code::NotFound);
    }
}
