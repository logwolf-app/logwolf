package main

import (
	"context"
	"logwolf-toolbox/data"
	"net"
	"net/rpc"
)

// projectPlan is the limits.PlanLookup the hosted edition's limits.Provider
// resolves plans with: it asks the logger, over a connection of its own, which
// plan the project's organization is on. It gives up when ctx is done.
func projectPlan(ctx context.Context, projectID string) (string, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", loggerRPCAddr())
	if err != nil {
		return "", err
	}
	client := rpc.NewClient(conn)
	defer client.Close()

	var plan string
	call := client.Go("RPCServer.ProjectPlan", &data.RPCProjectIDArgs{ID: projectID}, &plan, nil)
	select {
	case <-call.Done:
		return plan, call.Error
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// projectQuota is the limits.QuotaLookup the hosted edition's limits.Provider
// builds monthly quotas with: it asks the logger, over a connection of its own,
// which organization the project is in, its plan, and the events it has
// ingested this month. It gives up when ctx is done.
func projectQuota(ctx context.Context, projectID string) (data.ProjectQuota, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", loggerRPCAddr())
	if err != nil {
		return data.ProjectQuota{}, err
	}
	client := rpc.NewClient(conn)
	defer client.Close()

	var quota data.ProjectQuota
	call := client.Go("RPCServer.ProjectQuota", &data.RPCProjectIDArgs{ID: projectID}, &quota, nil)
	select {
	case <-call.Done:
		return quota, call.Error
	case <-ctx.Done():
		return data.ProjectQuota{}, ctx.Err()
	}
}
