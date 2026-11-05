package pirn

import (
	"context"

	"github.com/heedle-ai/bobbin"
	"github.com/heedle-ai/reed"
)

// heedle's labels (roadmap 9.2). A step's context carries its run labels
// (bobbin.WithLabels): the workflow that scheduled it and that workflow's
// run, from Temporal's activity info, and its node ID. pirn names each
// container's kind: an agent step's workspace is "agent", a skill step's
// "skill", a connector's MCP server "connector", an act step's browser
// "act".

// stepContext is ctx carrying the step's run labels: those it carries
// already (loom.Labelled sets them, node ID and all, where every step's
// activity starts), else Temporal's activity info with node.
func stepContext(ctx context.Context, node string) context.Context {
	if bobbin.LabelsFrom(ctx) != (bobbin.Labels{}) {
		return ctx
	}
	return bobbin.WithLabels(ctx, bobbin.StepLabels(ctx, node))
}

// visitLabels are ctx's run labels as reed's, for an act session's
// browser, with the worker's home (bobbin.HomeFrom, else the labels';
// roadmap 9.2.1).
func visitLabels(ctx context.Context) map[string]string {
	l := bobbin.LabelsFrom(ctx)
	home := bobbin.HomeFrom(ctx)
	if home == "" {
		home = l.Home
	}
	m := map[string]string{}
	for k, v := range map[string]string{reed.LabelWorkflow: l.Workflow, reed.LabelRun: l.Run, reed.LabelStep: l.Step, reed.LabelHome: home} {
		if v != "" {
			m[k] = v
		}
	}
	return m
}
