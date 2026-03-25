package api

// PaymentProvider defines a modular interface for payment/currency systems.
// Implement this interface to integrate any payment backend (e.g., Beans Bank,
// Stripe, in-game coins, etc.).
type PaymentProvider interface {
	// Name returns a human-readable name for this payment provider.
	Name() string

	// CreateGiftLink creates a redeemable gift worth `amount` units of currency.
	// Returns the gift code and a URL where the user can redeem it.
	// expiresIn is a duration string like "24h".
	CreateGiftLink(amount int, expiresIn string, message string) (*GiftLink, error)

	// GetGiftLink retrieves details about a gift link by its code.
	GetGiftLink(code string) (*GiftLink, error)

	// PaymentURL returns a URL that a user can visit to send `amount` to the merchant.
	// fromUser is the username of the payer.
	PaymentURL(fromUser string, amount int) string

	// GetTransactions returns recent incoming transactions to the merchant account.
	// Used for verifying payments.
	GetTransactions() ([]PaymentTransaction, error)

	// GetBalance returns the merchant's current balance.
	GetBalance() (int, error)

	// SendTransfer sends beans from the merchant account directly to a recipient user.
	// Used to pay out sellers after a marketplace sale, or to refund buyers on reversal.
	SendTransfer(toUser string, amount int) error
}

// GiftLink represents a redeemable gift created by the merchant.
type GiftLink struct {
	ID           int    `json:"id"`
	Code         string `json:"code"`
	Amount       int    `json:"amount"`
	Message      string `json:"message"`
	FromUsername string `json:"fromUsername"`
	RedeemURL    string `json:"redeemUrl"`
	Active       bool   `json:"active"`
	Redeemed     bool   `json:"redeemed"`
	RedeemedBy   string `json:"redeemedBy,omitempty"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"`
}

// PaymentTransaction represents an incoming payment.
type PaymentTransaction struct {
	ID        int    `json:"id"`
	FromUser  string `json:"fromUser"`
	ToUser    string `json:"toUser"`
	Amount    int    `json:"amount"`
	Timestamp string `json:"timestamp"`
}
