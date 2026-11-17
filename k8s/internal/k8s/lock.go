package k8s

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultLockTTL   = 30 * time.Second
	defaultLockRenew = 10 * time.Second
)

var errLockLost = errors.New("the installation's lock is no longer this target's")

func lockOwner() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Lock takes a renewable Lease of the installation. The target process releases it; a dead process loses it at TTL.
func (in *Installation) Lock(ctx context.Context) (func(context.Context) error, error) {
	if in.lockStop != nil {
		return nil, fmt.Errorf("%s: the installation is already locked by this target", in.installation)
	}
	owner, err := lockOwner()
	if err != nil {
		return nil, err
	}
	ttl, renew := in.lockTTL, in.lockRenew
	if ttl == 0 {
		ttl = defaultLockTTL
	}
	if renew == 0 {
		renew = defaultLockRenew
	}
	c, err := in.cluster()
	if err != nil {
		return nil, err
	}
	seconds := max(int32(ttl/time.Second), 1)
	for {
		now := time.Now().UTC()
		lease := &coordinationv1.Lease{}
		err = c.Get(ctx, client.ObjectKey{Namespace: in.namespace, Name: in.recordName()}, lease)
		if apierrors.IsNotFound(err) {
			lease = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: in.namespace, Name: in.recordName()}}
			err = nil
		} else if err == nil {
			live := lease.Spec.HolderIdentity != nil && lease.Spec.RenewTime != nil && lease.Spec.LeaseDurationSeconds != nil &&
				now.Before(lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds)*time.Second))
			if live && *lease.Spec.HolderIdentity != owner {
				return nil, fmt.Errorf("%s: the installation is locked by %s", in.installation, *lease.Spec.HolderIdentity)
			}
		}
		if err != nil {
			return nil, err
		}
		at := metav1.NewMicroTime(time.Now().UTC())
		lease.Spec = coordinationv1.LeaseSpec{
			HolderIdentity:       &owner,
			LeaseDurationSeconds: &seconds,
			AcquireTime:          &at,
			RenewTime:            &at,
		}
		if lease.ResourceVersion == "" {
			err = c.Create(ctx, lease)
		} else {
			err = c.Update(ctx, lease)
		}
		if err == nil {
			break
		}
		if !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	in.lockOwner = owner
	in.lockStop = make(chan struct{})
	in.lockDone = make(chan struct{})
	in.lockLost = make(chan struct{})
	go in.renewLock(renew, ttl)
	return in.unlockLock, nil
}

// LockLost closes when this target must stop its saga: ownership is gone or renewal failed through the lease's TTL.
func (in *Installation) LockLost() <-chan struct{} { return in.lockLost }

func (in *Installation) renewLock(every, ttl time.Duration) {
	defer close(in.lockDone)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	last := time.Now()
	for {
		select {
		case <-in.lockStop:
			return
		case at := <-ticker.C:
			err := in.renewLockOnce(at.UTC(), ttl)
			if err == nil {
				last = at
				continue
			}
			if !errors.Is(err, errLockLost) && time.Since(last) < ttl {
				continue
			}
			close(in.lockLost)
			return
		}
	}
}

func (in *Installation) renewLockOnce(at time.Time, ttl time.Duration) error {
	c, err := in.cluster()
	if err != nil {
		return err
	}
	lease := &coordinationv1.Lease{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: in.namespace, Name: in.recordName()}, lease); err != nil {
		return err
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != in.lockOwner {
		return errLockLost
	}
	seconds := max(int32(ttl/time.Second), 1)
	atMicro := metav1.NewMicroTime(at)
	lease.Spec.RenewTime = &atMicro
	lease.Spec.LeaseDurationSeconds = &seconds
	return c.Update(context.Background(), lease)
}

func (in *Installation) unlockLock(ctx context.Context) error {
	close(in.lockStop)
	<-in.lockDone
	in.lockStop, in.lockDone = nil, nil
	c, err := in.cluster()
	if err != nil {
		return err
	}
	lease := &coordinationv1.Lease{}
	err = c.Get(ctx, client.ObjectKey{Namespace: in.namespace, Name: in.recordName()}, lease)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != in.lockOwner {
		return nil
	}
	rv := lease.ResourceVersion
	if err := c.Delete(ctx, lease, client.Preconditions{ResourceVersion: &rv}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	in.lockOwner = ""
	return nil
}
