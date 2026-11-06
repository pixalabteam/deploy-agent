package agent

import (
	"context"
	"fmt"
)

// Scale changes the number of instances of an HTTP service. Scaling up starts new
// slots with the image of slot 0 (health-checked and added to nginx one by one);
// scaling down first removes the slot from nginx, then stops its container, so
// no request is routed to a container that is going away.
func (d *Deployer) Scale(ctx context.Context, t Target, target int) error {
	if t.Svc.Kind != KindHTTP {
		return fmt.Errorf("service %s is not an http service and cannot be scaled", t.Service)
	}
	if target < 1 || target > maxInstances {
		return fmt.Errorf("instance count must be between 1 and %d", maxInstances)
	}
	current := d.InstanceCount(t)
	switch {
	case target > current:
		return d.scaleUp(ctx, t, current, target)
	case target < current:
		return d.scaleDown(ctx, t, current, target)
	}
	return nil
}

func (d *Deployer) scaleUp(ctx context.Context, t Target, current, target int) error {
	image := d.instance(t, 0).Image
	if image == "" {
		return fmt.Errorf("no deployed image yet, deploy once before scaling")
	}
	for i := current; i < target; i++ {
		// Count is raised before deploying slot i so applyUpstream includes it;
		// on failure it goes back to the slots that really exist.
		if err := d.state.SetCount(t.Project.Name, t.Service, i+1); err != nil {
			return err
		}
		if err := d.deployInstance(ctx, t, i, image); err != nil {
			_ = d.state.SetCount(t.Project.Name, t.Service, i)
			return fmt.Errorf("instance #%d: %w", i+1, err)
		}
	}
	return nil
}

func (d *Deployer) scaleDown(ctx context.Context, t Target, current, target int) error {
	for i := current - 1; i >= target; i-- {
		removed := d.instance(t, i)
		if err := d.applyUpstream(ctx, t, i, -1, 0); err != nil {
			return fmt.Errorf("remove instance #%d from nginx: %w", i+1, err)
		}
		if err := d.state.SetCount(t.Project.Name, t.Service, i); err != nil {
			return err
		}
		if err := d.docker.RemoveForce(ctx, removed.ContainerName); err != nil {
			return fmt.Errorf("instance #%d left nginx but its container %s was not stopped (remove it manually): %w", i+1, removed.ContainerName, err)
		}
	}
	return nil
}
