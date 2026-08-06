// Package api holds the wire types shared by the agent and the cloud control
// plane: NodeStatus, EnrollRequest, ClusterState, HealthReport. Defined once
// here so the two sides can never drift; see CONTRIBUTING.md on widening what
// leaves the house.
package api
