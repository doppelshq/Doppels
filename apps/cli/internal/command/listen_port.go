package command

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"doppels.so/cli/internal/execution"
	"doppels.so/cli/internal/listener"
	"doppels.so/cli/internal/manifest"
)

// cliApprovalPort implements listener.ApprovalPort on top of stdin
// interaction: the exact prompts `node up` showed before the listener
// extraction, now behind the headless decision boundary.
type cliApprovalPort struct {
	interaction                   *interaction
	manualOutputs, manualEvidence map[string]string
}

func (p *cliApprovalPort) DecideFulfillment(ctx context.Context, job listener.Job, queue listener.QueueInfo) (listener.Decision, error) {
	_ = ctx
	_ = job
	_ = queue
	decision, err := p.interaction.decideFulfillment()
	switch decision {
	case fulfillApprove:
		return listener.DecisionApprove, err
	case fulfillReject:
		return listener.DecisionReject, err
	case fulfillSkip:
		return listener.DecisionSkip, err
	case fulfillBackground:
		return listener.DecisionBackground, err
	default:
		return listener.DecisionReject, err
	}
}

func (p *cliApprovalPort) ApproveStep(ctx context.Context, request execution.ApprovalRequest) (bool, error) {
	return p.interaction.approve(ctx, request)
}

func (p *cliApprovalPort) FulfillManual(ctx context.Context, request execution.ManualRequest) (execution.ManualResult, error) {
	return p.interaction.manual(p.manualOutputs, p.manualEvidence)(ctx, request)
}

func (p *cliApprovalPort) PickRecipe(_ context.Context, capability string, matches []manifest.RecipeDefinition) (manifest.RecipeDefinition, error) {
	style := newTermStyle(p.interaction.output)
	fmt.Fprintln(p.interaction.output)
	fmt.Fprintln(p.interaction.output, "  "+style.bold("Multiple Recipes provide "+capability+". Pick one:"))
	for index, match := range matches {
		fmt.Fprintf(p.interaction.output, "  %s  %s@%s\n", style.label(fmt.Sprintf("[%d]", index+1)), match.Value.Metadata.Name, match.Value.Metadata.Version)
	}
	for {
		answer, err := p.interaction.read("  Recipe › ")
		if err != nil {
			return manifest.RecipeDefinition{}, err
		}
		answer = strings.TrimSpace(answer)
		if n, convErr := strconv.Atoi(answer); convErr == nil && n >= 1 && n <= len(matches) {
			return matches[n-1], nil
		}
		for _, match := range matches {
			name := match.Value.Metadata.Name
			full := name + "@" + match.Value.Metadata.Version
			if answer == name || answer == full {
				return match, nil
			}
		}
		fmt.Fprintln(p.interaction.output, "  "+style.yellow("Not recognized. Enter a number or Recipe name."))
	}
}
