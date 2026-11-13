package hourglass

import "fmt"

// PWLocation is one public witnessing location ("carrinho").
type PWLocation struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Disabled bool   `json:"disabled"`
	Notes    string `json:"notes"`
}

// PWSlot is one publisher assignment in a public witnessing slot.
// The API stores one row per publisher, so a slot with two publishers
// returns two rows that share location, datetime and duration.
type PWSlot struct {
	ID         int     `json:"id"`
	LocationID int     `json:"location_id"`
	Datetime   string  `json:"datetime"`
	Duration   string  `json:"duration"`
	AssigneeID *int    `json:"assignee_id"`
	Notes      *string `json:"notes"`
}

// GetPWLocations lists the public witnessing locations, including disabled ones.
func (c *Client) GetPWLocations() ([]PWLocation, error) {
	var locations []PWLocation
	url := fmt.Sprintf("%s/scheduling/pw/locations?disabled=true", c.baseURL)
	if err := c.getJSON(url, &locations); err != nil {
		return nil, err
	}
	return locations, nil
}

// GetPWSchedule lists the public witnessing slots between two ISO dates.
func (c *Client) GetPWSchedule(start, end string) ([]PWSlot, error) {
	var slots []PWSlot
	url := fmt.Sprintf("%s/scheduling/pw/schedule/%s_%s", c.baseURL, start, end)
	if err := c.getJSON(url, &slots); err != nil {
		return nil, err
	}
	return slots, nil
}
