package server

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"kungfu.md/internal/admin"
	apperrors "kungfu.md/internal/errors"
	"kungfu.md/internal/middleware"
	"kungfu.md/internal/model"
	"kungfu.md/internal/repository"
)

// registerSamelabs mounts the server-rendered platform admin.
func (s *Server) registerSamelabs(r chi.Router) {
	r.Get(slBase+"/login", s.slLoginPage)
	r.Post(slBase+"/login", s.slLoginSubmit)
	r.Post(slBase+"/logout", s.slPost(s.slLogout))

	r.Get(slBase, s.slGet("dashboard", "Dashboard", "dashboard", s.slDashboard))
	r.Get(slBase+"/account", s.slGet("account", "My account", "account", func(*http.Request, *admin.Principal) (interface{}, error) { return nil, nil }))
	r.Post(slBase+"/account/password", s.slChangePassword)

	// operations
	r.Get(slBase+"/tasks", s.slGet("tasks", "Tasks", "tasks", s.slTasks))
	r.Get(slBase+"/tasks/{code}", s.slGet("tasks", "Task", "task", s.slTask))
	r.Post(slBase+"/tasks/{code}/close", s.slPost(s.slTaskClose))
	r.Post(slBase+"/tasks/{code}/pin", s.slPost(s.slTaskPin(true)))
	r.Post(slBase+"/tasks/{code}/unpin", s.slPost(s.slTaskPin(false)))

	r.Get(slBase+"/memories", s.slGet("memories", "Memories", "memories", s.slMemories))
	r.Get(slBase+"/memories/{code}", s.slGet("memories", "Memory", "memory", s.slMemory))
	r.Post(slBase+"/memories/{code}/unshare", s.slPost(s.slMemoryAction("unshare")))
	r.Post(slBase+"/memories/{code}/remove", s.slPost(s.slMemoryAction("remove")))

	r.Get(slBase+"/accounts", s.slGet("accounts", "Accounts", "accounts", s.slAccounts))
	r.Get(slBase+"/accounts/{id}", s.slGet("accounts", "Account", "account_detail", s.slAccount))
	r.Post(slBase+"/accounts/{id}/enable", s.slPost(s.slAccountStatus(true)))
	r.Post(slBase+"/accounts/{id}/disable", s.slPost(s.slAccountStatus(false)))

	// commerce
	r.Get(slBase+"/finance", s.slGet("finance", "Finance", "finance", s.slFinance))
	r.Get(slBase+"/finance/payments/{code}", s.slGet("finance", "Payment", "payment", s.slPayment))

	r.Get(slBase+"/store/products", s.slGet("products", "Store products", "products", s.slProducts))
	r.Post(slBase+"/store/products", s.slPost(s.slProductCreate))
	r.Get(slBase+"/store/products/{code}", s.slGet("products", "Store product", "product", s.slProduct))
	r.Post(slBase+"/store/products/{code}", s.slPost(s.slProductUpdate))
	r.Post(slBase+"/store/products/{code}/activate", s.slPost(s.slProductStatus("active")))
	r.Post(slBase+"/store/products/{code}/deactivate", s.slPost(s.slProductStatus("inactive")))

	r.Get(slBase+"/store/redemptions", s.slGet("redemptions", "Redemptions", "redemptions", s.slRedemptions))
	r.Get(slBase+"/store/redemptions/{code}", s.slGet("redemptions", "Redemption", "redemption", s.slRedemption))
	for _, act := range []string{"approve", "reject", "fulfill", "cancel"} {
		r.Post(slBase+"/store/redemptions/{code}/"+act, s.slPost(s.slRedemptionAction(act)))
	}

	// platform
	r.Get(slBase+"/settings/payment", s.slGet("payment", "Payment settings", "settings_payment", s.slPaymentSettings))
	r.Post(slBase+"/settings/payment", s.slPost(s.slPaymentSettingsSave))

	r.Get(slBase+"/admins", s.slGet("admins", "Admins", "admins", s.slAdmins))
	r.Post(slBase+"/admins", s.slPost(s.slAdminCreate))
	r.Get(slBase+"/admins/{id}", s.slGet("admins", "Admin", "admin_detail", s.slAdmin))
	r.Post(slBase+"/admins/{id}/profile", s.slPost(s.slAdminProfile))
	r.Post(slBase+"/admins/{id}/roles", s.slPost(s.slAdminRoles))
	r.Post(slBase+"/admins/{id}/password", s.slPost(s.slAdminPassword))
	r.Post(slBase+"/admins/{id}/enable", s.slPost(s.slAdminStatus(true)))
	r.Post(slBase+"/admins/{id}/disable", s.slPost(s.slAdminStatus(false)))
	r.Post(slBase+"/admins/{id}/force-logout", s.slPost(s.slAdminForceLogout))

	r.Get(slBase+"/roles", s.slGet("roles", "Roles", "roles", s.slRoles))
	r.Post(slBase+"/roles", s.slPost(s.slRoleCreate))
	r.Get(slBase+"/roles/{id}", s.slGet("roles", "Role", "role", s.slRole))
	r.Post(slBase+"/roles/{id}", s.slPost(s.slRoleUpdate))
	r.Post(slBase+"/roles/{id}/permissions", s.slPost(s.slRolePermissions))

	r.Get(slBase+"/sessions", s.slGet("sessions", "Sessions", "sessions", s.slSessions))
	r.Post(slBase+"/sessions/{id}/revoke", s.slPost(s.slSessionRevoke))

	r.Get(slBase+"/audit", s.slGet("audit", "Audit log", "audit", s.slAudit))
}

func notFound(what string) error { return apperrors.New(404, "NOT_FOUND", what+" not found") }

func pathInt(r *http.Request, name string) int64 {
	n, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// -- auth --

type slLoginData struct {
	Next     string
	Username string
}

func (s *Server) slLoginPage(w http.ResponseWriter, r *http.Request) {
	if p, _ := s.slSession(r); p != nil {
		http.Redirect(w, r, slSafeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.slRender(w, http.StatusOK, "login", &slView{Title: "Sign in", Query: r.URL.Query(), Flash: s.slTakeFlash(w, r),
		Data: slLoginData{Next: slSafeNext(r.URL.Query().Get("next"))}})
}

func (s *Server) slLoginSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, slFormLimit)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Form too large or malformed", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	next := slSafeNext(r.PostFormValue("next"))
	fail := func(status int, msg string) {
		s.slRender(w, status, "login", &slView{Title: "Sign in", Query: url.Values{}, Error: msg,
			Data: slLoginData{Next: next, Username: username}})
	}
	if username == "" || password == "" {
		fail(http.StatusBadRequest, "Enter your username and password.")
		return
	}
	ip := middleware.GetClientIP(r, s.TrustedProxies)
	if rl := s.RateLimiter.CheckAdminLogin(ip); !rl.Allowed {
		fail(http.StatusTooManyRequests, "Too many sign-in attempts. Wait a few minutes and try again.")
		return
	}
	res, err := admin.Login(r.Context(), s.Pool, admin.LoginInput{
		Username: username, Password: password, IPAddress: ip, UserAgent: truncateAdminUA(r.UserAgent()),
	})
	if err != nil {
		if ae, ok := apperrors.IsAppError(err); ok && ae.Code == "INVALID_CREDENTIALS" {
			admin.RecordFailedLogin(r.Context(), s.Pool, username, ip, truncateAdminUA(r.UserAgent()), "INVALID_CREDENTIALS")
			fail(http.StatusUnauthorized, "Username or password is incorrect.")
			return
		}
		fail(http.StatusInternalServerError, "Sign-in failed. Try again.")
		return
	}
	admin.SetAdminCookie(w, res.RawToken, middleware.IsHTTPS(r, s.TrustedProxies))
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) slLogout(r *http.Request, p *admin.Principal) (string, string, error) {
	return slBase + "/login", "Signed out.", admin.RevokeSession(r.Context(), s.Pool, p)
}

// slChangePassword is a form action that also ends the session (the
// domain revokes every session of the admin on password change).
func (s *Server) slChangePassword(w http.ResponseWriter, r *http.Request) {
	s.slPost(func(r *http.Request, p *admin.Principal) (string, string, error) {
		back := slBase + "/account"
		if r.PostFormValue("new_password") != r.PostFormValue("confirm_password") {
			return back, "", apperrors.New(400, "PASSWORD_MISMATCH", "The two new passwords do not match.")
		}
		if err := admin.ChangeOwnPassword(r.Context(), s.Pool, p, r.PostFormValue("current_password"), r.PostFormValue("new_password")); err != nil {
			return back, "", err
		}
		admin.ClearAdminCookie(w, middleware.IsHTTPS(r, s.TrustedProxies))
		return slBase + "/login", "Password changed. Sign in with the new password.", nil
	})(w, r)
}

// -- dashboard --

type slDashboardData struct {
	Counts      *repository.AdminDashboardCounts
	Finance     *admin.FinanceSummary
	Pending     []model.Redemption
	RecentTasks []repository.AdminTaskRow
}

func (s *Server) slDashboard(r *http.Request, p *admin.Principal) (interface{}, error) {
	ctx := r.Context()
	d := slDashboardData{}
	// RBAC: each domain's data is only loaded when the principal holds the
	// matching read permission; restricted admins must not see stats from
	// domains they cannot access. Counts feed the Accounts / Tasks /
	// Memories / Credits / Redemptions stat cards, so they load only when
	// at least one permission that renders a stats card is held. No new
	// query mechanism: the single existing Dashboard round trip is reused,
	// the template decides per card.
	if p.HasPermission("accounts.read") || p.HasPermission("tasks.read") ||
		p.HasPermission("memories.read") || p.HasPermission("finance.read") ||
		p.HasPermission("store.redemptions.read") {
		c, err := admin.Dashboard(ctx, s.Pool, p)
		if err != nil {
			return nil, err
		}
		d.Counts = c
	}
	if p.HasPermission("finance.read") {
		if fs, err := admin.GetFinanceSummary(ctx, s.Pool, p); err != nil {
			return nil, err
		} else {
			d.Finance = fs
		}
	}
	if p.HasPermission("store.redemptions.read") {
		if pd, _, err := admin.ListStoreRedemptions(ctx, s.Pool, p, admin.StoreRedemptionListFilter{Status: "pending_review", Page: 1, PageSize: 5}); err != nil {
			return nil, err
		} else {
			d.Pending = pd
		}
	}
	if p.HasPermission("tasks.read") {
		if rt, _, err := admin.ListTasks(ctx, s.Pool, p, repository.AdminTaskFilter{Page: 1, PageSize: 6}); err != nil {
			return nil, err
		} else {
			d.RecentTasks = rt
		}
	}
	return d, nil
}

// -- tasks --

type slList[T any] struct {
	Items []T
	Total int64
	Page  int
	Size  int
}

func (s *Server) slTasks(r *http.Request, p *admin.Principal) (interface{}, error) {
	q := r.URL.Query()
	page := slPage(q)
	items, total, err := admin.ListTasks(r.Context(), s.Pool, p, repository.AdminTaskFilter{
		Status: q.Get("status"), Q: q.Get("q"), BotID: slInt64(q.Get("bot_id")), Pinned: q.Get("pinned") == "1",
		Page: page, PageSize: slPageSize,
	})
	return slList[repository.AdminTaskRow]{Items: items, Total: total, Page: page, Size: slPageSize}, err
}

func (s *Server) slTask(r *http.Request, p *admin.Principal) (interface{}, error) {
	return admin.GetTask(r.Context(), s.Pool, p, chi.URLParam(r, "code"))
}

func (s *Server) slTaskClose(r *http.Request, p *admin.Principal) (string, string, error) {
	code := chi.URLParam(r, "code")
	return slBase + "/tasks/" + url.PathEscape(code), "Task closed. The owner sees your reason.",
		admin.CloseTask(r.Context(), s.Pool, p, code, r.PostFormValue("reason"))
}

func (s *Server) slTaskPin(pin bool) slAction {
	return func(r *http.Request, p *admin.Principal) (string, string, error) {
		code := chi.URLParam(r, "code")
		msg := "Task unpinned."
		if pin {
			msg = "Task pinned to the top of the homepage board."
		}
		return slBase + "/tasks/" + url.PathEscape(code), msg, admin.SetTaskPinned(r.Context(), s.Pool, p, code, pin)
	}
}

// -- memories --

func (s *Server) slMemories(r *http.Request, p *admin.Principal) (interface{}, error) {
	q := r.URL.Query()
	page := slPage(q)
	items, total, err := admin.ListMemories(r.Context(), s.Pool, p, repository.AdminMemoryFilter{
		Visibility: q.Get("visibility"), Status: q.Get("status"), Q: q.Get("q"), BotID: slInt64(q.Get("bot_id")),
		Page: page, PageSize: slPageSize,
	})
	return slList[repository.AdminMemoryRow]{Items: items, Total: total, Page: page, Size: slPageSize}, err
}

func (s *Server) slMemory(r *http.Request, p *admin.Principal) (interface{}, error) {
	return admin.GetMemory(r.Context(), s.Pool, p, chi.URLParam(r, "code"))
}

func (s *Server) slMemoryAction(action string) slAction {
	return func(r *http.Request, p *admin.Principal) (string, string, error) {
		code := chi.URLParam(r, "code")
		back := slBase + "/memories/" + url.PathEscape(code)
		if action == "remove" {
			return back, "Memory removed. It is private and deleted for its owner.", admin.RemoveMemory(r.Context(), s.Pool, p, code)
		}
		return back, "Memory is now private.", admin.UnshareMemory(r.Context(), s.Pool, p, code)
	}
}

// -- accounts --

func (s *Server) slAccounts(r *http.Request, p *admin.Principal) (interface{}, error) {
	q := r.URL.Query()
	page := slPage(q)
	status := q.Get("status")
	if status == "" {
		status = "all"
	}
	items, total, err := admin.ListAccounts(r.Context(), s.Pool, p, admin.AccountListFilter{Status: status, Q: q.Get("q"), Page: page, PageSize: slPageSize})
	return slList[admin.AccountView]{Items: items, Total: total, Page: page, Size: slPageSize}, err
}

type slAccountData struct {
	Account  *admin.AccountDetailView
	Tasks    []repository.AdminTaskRow
	Memories []repository.AdminMemoryRow
	Ledger   []admin.FinanceLedgerEntry
	Payments []admin.FinancePayment
}

func (s *Server) slAccount(r *http.Request, p *admin.Principal) (interface{}, error) {
	ctx, id := r.Context(), pathInt(r, "id")
	if id == 0 {
		return nil, notFound("Account")
	}
	a, err := admin.GetAccount(ctx, s.Pool, p, id)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, notFound("Account")
	}
	d := slAccountData{Account: a}
	if p.HasPermission("tasks.read") {
		if d.Tasks, _, err = admin.ListTasks(ctx, s.Pool, p, repository.AdminTaskFilter{BotID: id, Page: 1, PageSize: 10}); err != nil {
			return nil, err
		}
	}
	if p.HasPermission("memories.read") {
		if d.Memories, _, err = admin.ListMemories(ctx, s.Pool, p, repository.AdminMemoryFilter{BotID: id, Status: "all", Page: 1, PageSize: 10}); err != nil {
			return nil, err
		}
	}
	if p.HasPermission("finance.read") {
		if d.Ledger, _, err = admin.ListFinanceLedger(ctx, s.Pool, p, admin.FinanceLedgerFilter{BotID: id, Page: 1, PageSize: 20}); err != nil {
			return nil, err
		}
		if d.Payments, _, err = admin.ListFinancePayments(ctx, s.Pool, p, admin.FinancePaymentFilter{BotID: id, Page: 1, PageSize: 10}); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func (s *Server) slAccountStatus(enable bool) slAction {
	return func(r *http.Request, p *admin.Principal) (string, string, error) {
		id := pathInt(r, "id")
		back := slBase + "/accounts/" + strconv.FormatInt(id, 10)
		if enable {
			return back, "Account enabled.", admin.EnablePlatformAccount(r.Context(), s.Pool, p, id)
		}
		return back, "Account disabled. Its key and owner login stop working immediately.", admin.DisablePlatformAccount(r.Context(), s.Pool, p, id)
	}
}

// -- finance --

type slFinanceData struct {
	Tab         string
	Summary     *admin.FinanceSummary
	Payments    slList[admin.FinancePayment]
	Adjustments slList[admin.FinanceAdjustment]
	Ledger      slList[admin.FinanceLedgerEntry]
}

func (s *Server) slFinance(r *http.Request, p *admin.Principal) (interface{}, error) {
	ctx, q := r.Context(), r.URL.Query()
	page := slPage(q)
	sum, err := admin.GetFinanceSummary(ctx, s.Pool, p)
	if err != nil {
		return nil, err
	}
	d := slFinanceData{Tab: q.Get("tab"), Summary: sum}
	switch d.Tab {
	case "adjustments":
		items, total, err := admin.ListFinanceAdjustments(ctx, s.Pool, p, admin.FinanceAdjustmentFilter{
			Kind: q.Get("kind"), PaymentCode: strings.TrimSpace(q.Get("payment")), BotID: slInt64(q.Get("bot_id")), Page: page, PageSize: slPageSize})
		if err != nil {
			return nil, err
		}
		d.Adjustments = slList[admin.FinanceAdjustment]{items, total, page, slPageSize}
	case "ledger":
		items, total, err := admin.ListFinanceLedger(ctx, s.Pool, p, admin.FinanceLedgerFilter{
			BotID: slInt64(q.Get("bot_id")), Type: strings.TrimSpace(q.Get("type")), Page: page, PageSize: slPageSize})
		if err != nil {
			return nil, err
		}
		d.Ledger = slList[admin.FinanceLedgerEntry]{items, total, page, slPageSize}
	default:
		d.Tab = "payments"
		items, total, err := admin.ListFinancePayments(ctx, s.Pool, p, admin.FinancePaymentFilter{
			Status: q.Get("status"), BotID: slInt64(q.Get("bot_id")), Q: strings.TrimSpace(q.Get("q")), Page: page, PageSize: slPageSize})
		if err != nil {
			return nil, err
		}
		d.Payments = slList[admin.FinancePayment]{items, total, page, slPageSize}
	}
	return d, nil
}

func (s *Server) slPayment(r *http.Request, p *admin.Principal) (interface{}, error) {
	return admin.GetFinancePaymentDetail(r.Context(), s.Pool, p, chi.URLParam(r, "code"))
}

// -- store --

func (s *Server) slProducts(r *http.Request, p *admin.Principal) (interface{}, error) {
	q := r.URL.Query()
	page := slPage(q)
	status := q.Get("status")
	if status == "" {
		status = "all"
	}
	items, total, err := admin.ListStoreProducts(r.Context(), s.Pool, p, admin.StoreProductListFilter{Status: status, Q: q.Get("q"), Page: page, PageSize: slPageSize})
	return slList[model.StoreProduct]{Items: items, Total: total, Page: page, Size: slPageSize}, err
}

func (s *Server) slProduct(r *http.Request, p *admin.Principal) (interface{}, error) {
	return admin.GetStoreProduct(r.Context(), s.Pool, p, chi.URLParam(r, "code"))
}

// formCredits parses a whole positive credit amount from a form field.
func formCredits(r *http.Request, field string) (int64, error) {
	v := strings.TrimSpace(r.PostFormValue(field))
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, apperrors.New(400, "INVALID_PRICE", "Price must be a whole number of credits greater than zero.")
	}
	return n, nil
}

func (s *Server) slProductCreate(r *http.Request, p *admin.Principal) (string, string, error) {
	back := slBase + "/store/products"
	price, err := formCredits(r, "credits_price")
	if err != nil {
		return back, "", err
	}
	prod, err := admin.CreateStoreProduct(r.Context(), s.Pool, p, admin.StoreProductInput{
		Title: strings.TrimSpace(r.PostFormValue("title")), Description: strings.TrimSpace(r.PostFormValue("description")), CreditsPrice: price})
	if err != nil {
		return back, "", err
	}
	return back + "/" + url.PathEscape(prod.Code), "Product created and active.", nil
}

func (s *Server) slProductUpdate(r *http.Request, p *admin.Principal) (string, string, error) {
	code := chi.URLParam(r, "code")
	back := slBase + "/store/products/" + url.PathEscape(code)
	price, err := formCredits(r, "credits_price")
	if err != nil {
		return back, "", err
	}
	title := strings.TrimSpace(r.PostFormValue("title"))
	desc := strings.TrimSpace(r.PostFormValue("description"))
	_, err = admin.UpdateStoreProduct(r.Context(), s.Pool, p, code, admin.StoreProductPatch{Title: &title, Description: &desc, CreditsPrice: &price})
	return back, "Product saved.", err
}

func (s *Server) slProductStatus(status string) slAction {
	return func(r *http.Request, p *admin.Principal) (string, string, error) {
		code := chi.URLParam(r, "code")
		msg := "Product hidden from the store."
		if status == "active" {
			msg = "Product is live in the store."
		}
		_, err := admin.SetStoreProductStatus(r.Context(), s.Pool, p, code, status)
		return slBase + "/store/products/" + url.PathEscape(code), msg, err
	}
}

func (s *Server) slRedemptions(r *http.Request, p *admin.Principal) (interface{}, error) {
	q := r.URL.Query()
	page := slPage(q)
	status := q.Get("status")
	if status == "" {
		status = "all"
	}
	items, total, err := admin.ListStoreRedemptions(r.Context(), s.Pool, p, admin.StoreRedemptionListFilter{
		Status: status, BotID: slInt64(q.Get("bot_id")), Q: q.Get("q"), Page: page, PageSize: slPageSize})
	return slList[model.Redemption]{Items: items, Total: total, Page: page, Size: slPageSize}, err
}

func (s *Server) slRedemption(r *http.Request, p *admin.Principal) (interface{}, error) {
	return admin.GetStoreRedemption(r.Context(), s.Pool, p, chi.URLParam(r, "code"))
}

func (s *Server) slRedemptionAction(action string) slAction {
	return func(r *http.Request, p *admin.Principal) (string, string, error) {
		ctx, code := r.Context(), chi.URLParam(r, "code")
		back := slBase + "/store/redemptions/" + url.PathEscape(code)
		note := strings.TrimSpace(r.PostFormValue("note"))
		var err error
		msg := ""
		switch action {
		case "approve":
			_, err = admin.ApproveStoreRedemption(ctx, s.Pool, p, code, note)
			msg = "Redemption approved. Fulfil it once delivered."
		case "reject":
			_, err = admin.RejectStoreRedemption(ctx, s.Pool, p, code, note)
			msg = "Redemption rejected and credits refunded."
		case "fulfill":
			_, err = admin.FulfillStoreRedemption(ctx, s.Pool, p, code, note)
			msg = "Redemption marked fulfilled."
		case "cancel":
			_, err = admin.CancelStoreRedemption(ctx, s.Pool, p, code)
			msg = "Redemption cancelled and credits refunded."
		}
		return back, msg, err
	}
}

// -- payment settings --

type slPaymentData struct {
	View *admin.CreemSettingsView
	Rows []admin.CreemPackage // stored packages plus blank rows to add more
}

func (s *Server) slPaymentSettings(r *http.Request, p *admin.Principal) (interface{}, error) {
	v, err := admin.GetCreemSettings(r.Context(), s.Pool, p, s.secretBox)
	if err != nil {
		return nil, err
	}
	rows := append([]admin.CreemPackage{}, v.Packages...)
	for i := 0; i < 2 || len(rows) < 3; i++ {
		rows = append(rows, admin.CreemPackage{})
	}
	return slPaymentData{View: v, Rows: rows}, nil
}

func (s *Server) slPaymentSettingsSave(r *http.Request, p *admin.Principal) (string, string, error) {
	back := slBase + "/settings/payment"
	codes, products, credits := r.PostForm["pkg_code"], r.PostForm["pkg_product"], r.PostForm["pkg_credits"]
	if len(codes) != len(products) || len(codes) != len(credits) {
		return back, "", apperrors.New(400, "INVALID_SETTINGS", "Package rows are incomplete.")
	}
	var pkgs []admin.CreemPackage
	for i := range codes {
		c, pr, cr := strings.TrimSpace(codes[i]), strings.TrimSpace(products[i]), strings.TrimSpace(credits[i])
		if c == "" && pr == "" && cr == "" {
			continue // blank row
		}
		n, err := strconv.ParseInt(cr, 10, 64)
		if err != nil {
			return back, "", apperrors.New(400, "INVALID_SETTINGS", "Credits for package "+strconv.Quote(c)+" must be a whole number.")
		}
		pkgs = append(pkgs, admin.CreemPackage{Code: c, ProductID: pr, Credits: n})
	}
	err := admin.SaveCreemSettings(r.Context(), s.Pool, p, s.secretBox, admin.CreemSettingsInput{
		Enabled:       r.PostFormValue("enabled") == "1",
		Mode:          r.PostFormValue("mode"),
		SuccessURL:    r.PostFormValue("success_url"),
		Packages:      pkgs,
		APIKey:        r.PostFormValue("api_key"),
		WebhookSecret: r.PostFormValue("webhook_secret"),
	})
	if err == nil {
		s.invalidateCreemSettings()
	}
	return back, "Payment settings saved. They apply immediately.", err
}

// -- admins --

type slAdminsData struct {
	Users []*admin.AdminUserView
}

func (s *Server) slAdmins(r *http.Request, p *admin.Principal) (interface{}, error) {
	users, err := admin.ListUsers(r.Context(), s.Pool, p)
	return slAdminsData{Users: users}, err
}

func (s *Server) slAdminCreate(r *http.Request, p *admin.Principal) (string, string, error) {
	back := slBase + "/admins"
	a, err := admin.CreateAdmin(r.Context(), s.Pool, p, admin.CreateAdminInput{
		Username: r.PostFormValue("username"), DisplayName: strings.TrimSpace(r.PostFormValue("display_name")),
		Password: r.PostFormValue("password"), Actor: p.Admin,
	})
	if err != nil {
		return back, "", err
	}
	return back + "/" + strconv.FormatInt(a.ID, 10), "Admin created. Assign roles below.", nil
}

type slAdminData struct {
	User  *admin.AdminUserView
	Roles []*admin.AdminRoleView
	Self  bool
}

func (s *Server) slAdmin(r *http.Request, p *admin.Principal) (interface{}, error) {
	id := pathInt(r, "id")
	if id == 0 {
		return nil, notFound("Admin")
	}
	u, err := admin.GetUser(r.Context(), s.Pool, p, id)
	if err != nil {
		return nil, err
	}
	d := slAdminData{User: u, Self: id == p.Admin.ID}
	if p.HasPermission("admin.roles.read") {
		if d.Roles, err = admin.ListRoles(r.Context(), s.Pool, p); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func adminBack(r *http.Request) string {
	return slBase + "/admins/" + strconv.FormatInt(pathInt(r, "id"), 10)
}

func (s *Server) slAdminProfile(r *http.Request, p *admin.Principal) (string, string, error) {
	_, err := admin.UpdateAdminDisplayName(r.Context(), s.Pool, p, pathInt(r, "id"), strings.TrimSpace(r.PostFormValue("display_name")))
	return adminBack(r), "Display name saved.", err
}

func (s *Server) slAdminRoles(r *http.Request, p *admin.Principal) (string, string, error) {
	var ids []int64
	for _, v := range r.PostForm["role"] {
		if id := slInt64(v); id > 0 {
			ids = append(ids, id)
		}
	}
	return adminBack(r), "Roles saved. They apply to active sessions immediately.",
		admin.SetAdminRoles(r.Context(), s.Pool, p, pathInt(r, "id"), ids)
}

func (s *Server) slAdminPassword(r *http.Request, p *admin.Principal) (string, string, error) {
	return adminBack(r), "Password reset. That admin was signed out everywhere.",
		admin.ResetAdminPassword(r.Context(), s.Pool, p, pathInt(r, "id"), r.PostFormValue("password"))
}

func (s *Server) slAdminStatus(enable bool) slAction {
	return func(r *http.Request, p *admin.Principal) (string, string, error) {
		if enable {
			return adminBack(r), "Admin enabled.", admin.EnableAdmin(r.Context(), s.Pool, p, pathInt(r, "id"))
		}
		return adminBack(r), "Admin disabled and signed out.", admin.DisableAdmin(r.Context(), s.Pool, p, pathInt(r, "id"))
	}
}

func (s *Server) slAdminForceLogout(r *http.Request, p *admin.Principal) (string, string, error) {
	return adminBack(r), "All sessions of that admin were ended.", admin.ForceLogoutAdmin(r.Context(), s.Pool, p, pathInt(r, "id"))
}

// -- roles --

type slRolesData struct {
	Roles []*admin.AdminRoleView
}

func (s *Server) slRoles(r *http.Request, p *admin.Principal) (interface{}, error) {
	roles, err := admin.ListRoles(r.Context(), s.Pool, p)
	return slRolesData{Roles: roles}, err
}

func (s *Server) slRoleCreate(r *http.Request, p *admin.Principal) (string, string, error) {
	back := slBase + "/roles"
	role, err := admin.CreateRole(r.Context(), s.Pool, p, admin.CreateRoleInput{
		Code: strings.TrimSpace(r.PostFormValue("code")), Name: strings.TrimSpace(r.PostFormValue("name")),
		Description: strings.TrimSpace(r.PostFormValue("description")),
	})
	if err != nil {
		return back, "", err
	}
	return back + "/" + strconv.FormatInt(role.ID, 10), "Role created. Choose its permissions below.", nil
}

type slRoleData struct {
	Role        *admin.AdminRoleView
	Permissions []*model.AdminPermission
}

func (s *Server) slRole(r *http.Request, p *admin.Principal) (interface{}, error) {
	id := pathInt(r, "id")
	if id == 0 {
		return nil, notFound("Role")
	}
	role, err := admin.GetRole(r.Context(), s.Pool, p, id)
	if err != nil {
		return nil, err
	}
	perms, err := admin.ListPermissions(r.Context(), s.Pool, p)
	return slRoleData{Role: role, Permissions: perms}, err
}

func roleBack(r *http.Request) string {
	return slBase + "/roles/" + strconv.FormatInt(pathInt(r, "id"), 10)
}

func (s *Server) slRoleUpdate(r *http.Request, p *admin.Principal) (string, string, error) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	desc := strings.TrimSpace(r.PostFormValue("description"))
	status := r.PostFormValue("status")
	_, err := admin.UpdateRole(r.Context(), s.Pool, p, pathInt(r, "id"), admin.RolePatch{Name: &name, Description: &desc, Status: &status})
	return roleBack(r), "Role saved.", err
}

func (s *Server) slRolePermissions(r *http.Request, p *admin.Principal) (string, string, error) {
	perms := r.PostForm["permission"]
	if perms == nil {
		perms = []string{}
	}
	return roleBack(r), "Permissions saved. They apply to active sessions immediately.",
		admin.SetRolePermissions(r.Context(), s.Pool, p, pathInt(r, "id"), perms)
}

// -- sessions & audit --

type slSessionsData struct {
	Sessions []*model.AdminSessionInfo
	Current  int64
}

func (s *Server) slSessions(r *http.Request, p *admin.Principal) (interface{}, error) {
	list, err := admin.ListSessions(r.Context(), s.Pool, p)
	return slSessionsData{Sessions: list, Current: p.Session.ID}, err
}

func (s *Server) slSessionRevoke(r *http.Request, p *admin.Principal) (string, string, error) {
	_, err := admin.RevokeSessionByID(r.Context(), s.Pool, p, pathInt(r, "id"))
	return slBase + "/sessions", "Session ended.", err
}

func (s *Server) slAudit(r *http.Request, p *admin.Principal) (interface{}, error) {
	q := r.URL.Query()
	f := admin.AuditFilter{
		ActorUsername: strings.TrimSpace(q.Get("actor")), Action: strings.TrimSpace(q.Get("action")),
		TargetType: strings.TrimSpace(q.Get("target_type")), TargetID: strings.TrimSpace(q.Get("target_id")),
		Page: slPage(q), PageSize: slPageSize,
	}
	switch q.Get("success") {
	case "true":
		t := true
		f.Success = &t
	case "false":
		b := false
		f.Success = &b
	}
	return admin.ExploreAudit(r.Context(), s.Pool, p, f)
}
