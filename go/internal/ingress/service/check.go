// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/store"
)

// refusalText is why the certificate serial of ingress name may not
// connect, in the words the ingress reads (PERMISSION_DENIED on every
// call, §5 of the ingress's design), or "" when nothing refuses it. A
// superseded certificate is refused once its revocation is in effect;
// within its grace it is not. The ingress declares its identity lost at
// once only on "was removed" (D19).
func refusalText(name, serial string, c store.IngressCertCheck) string {
	switch {
	case !c.Found:
		return fmt.Sprintf("certificate %s is not ingress %s's", serial, name)
	case c.IngressStatus == model.IngressRemoved:
		return fmt.Sprintf("ingress %s was removed; join it again", name)
	case c.Revoked() && c.RevokedReason == store.RevokedSuperseded:
		return fmt.Sprintf("certificate %s of ingress %s was superseded; reconnect with the newest certificate", serial, name)
	case c.Revoked():
		return fmt.Sprintf("certificate %s of ingress %s was revoked (%s); join it again", serial, name, c.RevokedReason)
	case c.Expired():
		return fmt.Sprintf("certificate %s of ingress %s expired; join it again", serial, name)
	}
	return ""
}

// CheckIngress is nodelink.Config.CheckIngress: whether the certificate
// serial of ingress name may connect from peerAddr, decided in one
// statement on the database clock, never from a cache. It returns the
// certificate's revocation time when one lies ahead (a superseded
// certificate in its grace), at which nodelink closes the connection. A
// definitive refusal is a nodelink.IngressRefusal with the text every call
// on the connection is answered with: an unknown certificate or one of
// another ingress, a removed ingress, a revocation in effect (superseded
// or not), an expired certificate. A database error is transient.
func (s *Service) CheckIngress(ctx context.Context, name, serial, peerAddr string) (time.Time, error) {
	c, err := s.st.CheckIngressCert(ctx, name, serial)
	if err != nil {
		return time.Time{}, fmt.Errorf("check certificate %s of ingress %s: %w", serial, name, err)
	}
	if text := refusalText(name, serial, c); text != "" {
		return time.Time{}, nodelink.IngressRefusal("%s", text)
	}
	return c.RevokeAhead(), nil
}

// checkInTx checks, in a transaction, that the certificate serial of
// ingress name still passes CheckIngress's conditions (RenewCertificate,
// Hello): a connection is checked at accept and re-checked when the
// ingress changes, but a call may come between a change and its re-check.
// It returns PERMISSION_DENIED with the refusal's text, or a database
// error.
func checkInTx(ctx context.Context, tx *store.Store, name, serial string) error {
	c, err := tx.CheckIngressCert(ctx, name, serial)
	if err != nil {
		return err
	}
	if text := refusalText(name, serial, c); text != "" {
		return status.Error(codes.PermissionDenied, text)
	}
	return nil
}
