package pages

// Count returns how many pages are needed to show total items, size per page.
func Count(total, size int) int {
	if size <= 0 {
		return 0
	}
	return total / size
}
