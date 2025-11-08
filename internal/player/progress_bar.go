package player

func ProgressBar(width int, progress float64) string {
	if width <= 0 {
		return ""
	}
	if progress < 0 {
		progress = 0
	}
	if progress > 1 {
		progress = 1
	}
	dot := int(float64(width) * progress)
	if dot >= width {
		dot = width - 1
	}
	out := make([]rune, 0, width*2)
	for i := 0; i < width; i++ {
		if i == dot {
			out = append(out, '🔘')
		} else {
			out = append(out, '▬')
		}
	}
	return string(out)
}
