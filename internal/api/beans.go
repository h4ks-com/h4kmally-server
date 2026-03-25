package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// BeansProvider implements PaymentProvider for the h4ks Beans Bank API.
type BeansProvider struct {
	apiURL   string // e.g. "https://beans.h4ks.com/api/v1"
	siteURL  string // e.g. "https://beans.h4ks.com"
	apiToken string // Bearer token for the merchant account
	merchant string // merchant username (for payment URLs)
	client   *http.Client
}

// NewBeansProvider creates a new Beans Bank payment provider.
func NewBeansProvider(siteURL, apiToken, merchant string) *BeansProvider {
	siteURL = strings.TrimRight(siteURL, "/")
	return &BeansProvider{
		apiURL:   siteURL + "/api/v1",
		siteURL:  siteURL,
		apiToken: apiToken,
		merchant: merchant,
		client:   &http.Client{Timeout: 15 * time.Second},
	}
}

func (bp *BeansProvider) Name() string {
	return "Beans Bank"
}

// doRequest makes an authenticated API request.
func (bp *BeansProvider) doRequest(method, path string, body interface{}) ([]byte, int, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, bp.apiURL+path, bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bp.apiToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := bp.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	return respBody, resp.StatusCode, nil
}

// CreateGiftLink creates a gift link that escrows beans from the merchant account.
func (bp *BeansProvider) CreateGiftLink(amount int, expiresIn string, message string) (*GiftLink, error) {
	reqBody := map[string]interface{}{
		"amount":  amount,
		"message": message,
	}
	if expiresIn != "" {
		reqBody["expires_in"] = expiresIn
	}

	respBody, status, err := bp.doRequest("POST", "/giftlinks", reqBody)
	if err != nil {
		return nil, err
	}
	if status != 200 && status != 201 {
		return nil, fmt.Errorf("create gift link: HTTP %d: %s", status, string(respBody))
	}

	var apiResp struct {
		ID           int    `json:"id"`
		Code         string `json:"code"`
		Amount       int    `json:"amount"`
		Message      string `json:"message"`
		FromUsername string `json:"from_username"`
		Active       bool   `json:"active"`
		RedeemedBy   string `json:"redeemed_by"`
		RedeemedAt   int64  `json:"redeemed_at"`
		ExpiresAt    int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("parse gift link response: %w", err)
	}

	return &GiftLink{
		ID:           apiResp.ID,
		Code:         apiResp.Code,
		Amount:       apiResp.Amount,
		Message:      apiResp.Message,
		FromUsername: apiResp.FromUsername,
		RedeemURL:    bp.siteURL + "/gift/" + apiResp.Code,
		Active:       apiResp.Active,
		Redeemed:     apiResp.RedeemedAt != 0,
		RedeemedBy:   apiResp.RedeemedBy,
		ExpiresAt:    apiResp.ExpiresAt,
	}, nil
}

// GetGiftLink retrieves information about a gift link.
func (bp *BeansProvider) GetGiftLink(code string) (*GiftLink, error) {
	respBody, status, err := bp.doRequest("GET", "/gift/"+code, nil)
	if err != nil {
		return nil, err
	}
	if status == 404 {
		return nil, fmt.Errorf("gift link not found")
	}
	if status != 200 {
		return nil, fmt.Errorf("get gift link: HTTP %d: %s", status, string(respBody))
	}

	var apiResp struct {
		ID           int    `json:"id"`
		Code         string `json:"code"`
		Amount       int    `json:"amount"`
		Message      string `json:"message"`
		FromUsername string `json:"from_username"`
		Active       bool   `json:"active"`
		RedeemedBy   string `json:"redeemed_by"`
		RedeemedAt   int64  `json:"redeemed_at"`
		ExpiresAt    int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("parse gift link response: %w", err)
	}

	return &GiftLink{
		ID:           apiResp.ID,
		Code:         apiResp.Code,
		Amount:       apiResp.Amount,
		Message:      apiResp.Message,
		FromUsername: apiResp.FromUsername,
		RedeemURL:    bp.siteURL + "/gift/" + apiResp.Code,
		Active:       apiResp.Active,
		Redeemed:     apiResp.RedeemedAt != 0,
		RedeemedBy:   apiResp.RedeemedBy,
		ExpiresAt:    apiResp.ExpiresAt,
	}, nil
}

// PaymentURL returns a URL where the user can pay the merchant.
// Format: https://beans.h4ks.com/transfer/{fromUser}/{merchant}/{amount}
func (bp *BeansProvider) PaymentURL(fromUser string, amount int) string {
	return fmt.Sprintf("%s/transfer/%s/%s/%d", bp.siteURL, fromUser, bp.merchant, amount)
}

// GetTransactions returns the merchant's recent transaction history.
// Fetches all pages to avoid missing transactions that scroll off page 1.
func (bp *BeansProvider) GetTransactions() ([]PaymentTransaction, error) {
	var allTxns []PaymentTransaction

	// Fetch up to 10 pages (safety limit) to get all recent transactions
	for page := 1; page <= 10; page++ {
		path := fmt.Sprintf("/transactions?page=%d", page)
		respBody, status, err := bp.doRequest("GET", path, nil)
		if err != nil {
			if page == 1 {
				return nil, err
			}
			break // stop paginating on error for subsequent pages
		}
		if status != 200 {
			if page == 1 {
				return nil, fmt.Errorf("get transactions: HTTP %d: %s", status, string(respBody))
			}
			break
		}

		var apiResp []struct {
			ID        int    `json:"id"`
			Amount    int    `json:"amount"`
			FromUser  string `json:"from_user"`
			ToUser    string `json:"to_user"`
			Timestamp string `json:"timestamp"`
		}
		if err := json.Unmarshal(respBody, &apiResp); err != nil {
			if page == 1 {
				return nil, fmt.Errorf("parse transactions: %w", err)
			}
			break
		}

		if len(apiResp) == 0 {
			break // no more pages
		}

		for _, t := range apiResp {
			allTxns = append(allTxns, PaymentTransaction{
				ID:        t.ID,
				FromUser:  t.FromUser,
				ToUser:    t.ToUser,
				Amount:    t.Amount,
				Timestamp: t.Timestamp,
			})
		}

		// If this page returned fewer items than typical page size, we've reached the end
		if len(apiResp) < 10 {
			break
		}
	}

	return allTxns, nil
}

// GetBalance returns the merchant's wallet balance.
func (bp *BeansProvider) GetBalance() (int, error) {
	respBody, status, err := bp.doRequest("GET", "/wallet", nil)
	if err != nil {
		return 0, err
	}
	if status != 200 {
		return 0, fmt.Errorf("get balance: HTTP %d: %s", status, string(respBody))
	}

	var wallet struct {
		BeanAmount int `json:"bean_amount"`
	}
	if err := json.Unmarshal(respBody, &wallet); err != nil {
		return 0, fmt.Errorf("parse wallet: %w", err)
	}
	return wallet.BeanAmount, nil
}
