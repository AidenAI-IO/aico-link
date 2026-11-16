package mnk

import "context"

// ValidateSystemAction restricts native navigation to supported commands.
func ValidateSystemAction(action string) error {
	if action != "home" {
		return InvalidArgumentsf("unsupported system action %q", action)
	}
	return nil
}

func (p *HTTPProvider) SupportsSystemAction(ctx context.Context, action string) (bool, error) {
	if err := ValidateSystemAction(action); err != nil {
		return false, err
	}
	caps, err := p.queryCapabilities(ctx)
	if err != nil {
		return false, err
	}
	for _, supported := range caps.SystemActions {
		if supported == action {
			return true, nil
		}
	}
	return false, nil
}

func (p *HTTPProvider) SystemAction(ctx context.Context, action string) error {
	if err := ValidateSystemAction(action); err != nil {
		return err
	}
	// Never replay failed navigation as a gesture: delivery may be partial.
	return p.sendRequest(ctx, MNKRequest{Operation: "system_action", SystemAction: &SystemActionParams{Action: action}})
}
