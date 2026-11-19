package devicecli

import (
	"fmt"
	"image"
	"strconv"
	"strings"
)

// LookOptions controls local screenshot storage without changing the tool HTTP input.
type LookOptions struct {
	Dir    string
	Region *image.Rectangle
}

func ParseLookOptions(args []string) (LookOptions, error) {
	opts := LookOptions{Dir: "/tmp"}
	hasDir := false
	for len(args) > 0 {
		switch args[0] {
		case "--dir":
			if hasDir || len(args) < 2 || strings.TrimSpace(args[1]) == "" || strings.HasPrefix(args[1], "--") {
				return opts, fmt.Errorf("look expects one directory after --dir")
			}
			opts.Dir, hasDir = args[1], true
			args = args[2:]
		case "--region":
			if opts.Region != nil || len(args) < 5 {
				return opts, fmt.Errorf("look expects one region: --region X1 Y1 X2 Y2")
			}
			var coords [4]int
			for i, value := range args[1:5] {
				n, err := strconv.Atoi(value)
				if err != nil || n < 0 || n > 1000 {
					return opts, fmt.Errorf("region coordinates must be integers between 0 and 1000")
				}
				coords[i] = n
			}
			region := image.Rectangle{Min: image.Pt(coords[0], coords[1]), Max: image.Pt(coords[2], coords[3])}
			if region.Empty() {
				return opts, fmt.Errorf("region must have X1 < X2 and Y1 < Y2")
			}
			opts.Region = &region
			args = args[5:]
		default:
			return opts, fmt.Errorf("unknown look argument: %s", args[0])
		}
	}
	return opts, nil
}
