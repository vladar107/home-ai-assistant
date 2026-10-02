package google

import "errors"

// Product is a Google service an account can be switched on for.
type Product string

const (
	ProductMail     Product = "mail"
	ProductCalendar Product = "calendar"
)

// Account is a connected Google account as the product connectors see it: Google's stable
// subject id and the address shown to the user and the model.
type Account struct {
	ID      string
	Address string
}

// ErrNotConnected means the account is not connected for this principal, the product is switched
// off or not granted for it, or Google no longer accepts its grant.
var ErrNotConnected = errors.New("the Google account is not connected")
