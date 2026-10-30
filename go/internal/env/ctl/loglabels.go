// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"strconv"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// The labels of a pod's container logs (ADR 0026), set at the insert of
// its row (sandboxes.log_labels) and sent with its create.

// instanceLogLabels are those of an environment's instance; attempt > 0
// makes them a job's run's.
func instanceLogLabels(owner string, id podid.ID, envID uuid.UUID, kind, object string, replica, release, attempt int) map[string]string {
	l := map[string]string{
		model.LogTenant: owner, model.LogInstance: id.String(), model.LogEnvironment: envID.String(),
		model.LogKind: kind, model.LogObject: object, model.LogReplica: strconv.Itoa(replica),
		model.LogRelease: strconv.Itoa(release),
	}
	if attempt > 0 {
		l[model.LogAttempt] = strconv.Itoa(attempt)
	}
	return l
}

// tenantJobLogLabels are those of a tenant's job's pod, its one attempt;
// days > 0 is the retention the job asked.
func tenantJobLogLabels(owner string, id podid.ID, days int) map[string]string {
	l := map[string]string{
		model.LogTenant: owner, model.LogInstance: id.String(), model.LogJob: id.String(), model.LogAttempt: "1",
	}
	if days > 0 {
		l[model.LogRetentionDays] = strconv.Itoa(days)
	}
	return l
}
