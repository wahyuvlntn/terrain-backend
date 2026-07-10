package api

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"afdd/internal/db"
	"afdd/internal/httpx"
)

func (s *Server) registerNotification() {
	p := "notification/"
	// In-app notifications.
	s.handle(p+"getnotifications", AccessUser, s.notifList)
	s.handle(p+"getunreadnotificationcounts", AccessUser, s.notifUnreadCount)
	s.handle(p+"updatenotifreadstatus", AccessUser, s.notifSetRead)
	s.handle(p+"readallnotif", AccessUser, s.notifReadAll)
	// Notification manager (email).
	s.handle(p+"getemailserverdetails", AccessUser, s.nmEmailDetails)
	s.handle(p+"registeremailnotification", AccessUser, s.nmRegisterEmail)
	s.handle(p+"getusergroups", AccessUser, s.nmUserGroups)
	s.handle(p+"getlocationoptions", AccessUser, s.nmLocationOptions)
	s.handle(p+"getnotificationcategories", AccessUser, s.nmCategories)
	s.handle(p+"getassignedcategoriesperusergroup", AccessUser, s.nmAssignedCategories)
	s.handle(p+"registerusergrouptoemailnotification", AccessUser, s.nmRegisterGroup)
	s.handle(p+"getusers", AccessUser, s.nmUsers)
	s.handle(p+"getunsubscribedusers", AccessUser, s.nmUnsubscribed)
	s.handle(p+"submitunsubscribedusers", AccessUser, s.nmSubmitUnsubscribed)
}

// notifVisibility returns a SQL condition (on alias n) restricting
// notifications to those the current user may see, plus its arguments
// starting at placeholder $start.
func (s *Server) notifVisibility(c *Ctx, start int) (string, []any, error) {
	if c.User.IsSuperuser() {
		return "TRUE", nil, nil
	}
	rows, err := s.DB.Query(c, `SELECT alias, COALESCE(permission_group, '') FROM core.notification_categories`)
	if err != nil {
		return "", nil, err
	}
	allowed := []string{}
	for rows.Next() {
		var alias, group string
		if err := rows.Scan(&alias, &group); err != nil {
			rows.Close()
			return "", nil, err
		}
		if group == "" || c.User.Can(group, "read") {
			allowed = append(allowed, alias)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	scope, err := c.Scope()
	if err != nil {
		return "", nil, err
	}
	cond := fmt.Sprintf(`n.category = ANY($%d) AND (n.recipients IS NULL OR cardinality(n.recipients) = 0
		OR EXISTS (SELECT 1 FROM unnest(n.recipients) r WHERE lower(r) = lower($%d)))`, start, start+1)
	args := []any{allowed, c.User.Username}
	if !scope.Unrestricted {
		cond += fmt.Sprintf(` AND (n.location_id IS NULL OR n.location_id = ANY($%d))`, start+2)
		args = append(args, emptyIfNil(scope.LocationIDs()))
	}
	return cond, args, nil
}

func (s *Server) notifList(c *Ctx) (any, error) {
	page, limit := c.page(1), c.limit(10, 200)
	cond, args, err := s.notifVisibility(c, 2)
	if err != nil {
		return nil, err
	}
	args = append([]any{c.User.ID}, args...)
	where := []string{cond}
	if t := c.timeParam("start_ts"); t != nil {
		args = append(args, *t)
		where = append(where, fmt.Sprintf("n.created_at >= $%d", len(args)))
	}
	if t := c.timeParam("end_ts"); t != nil {
		end := *t
		if raw := c.F.Str("end_ts"); !strings.Contains(raw, "T") || strings.Contains(raw, "T00:00:00") {
			end = end.AddDate(0, 0, 1) // the end date is inclusive
		}
		args = append(args, end)
		where = append(where, fmt.Sprintf("n.created_at < $%d", len(args)))
	}
	if q := c.F.StrTrim("search"); q != "" {
		args = append(args, ilike(q))
		where = append(where, fmt.Sprintf("(n.message ILIKE $%[1]d OR n.category ILIKE $%[1]d OR l.name ILIKE $%[1]d)", len(args)))
	}
	if strings.EqualFold(c.F.StrTrim("notif_type"), "unread") {
		where = append(where, "nr.notification_id IS NULL")
	}
	args = append(args, limit+1, (page-1)*limit)
	rows, err := s.DB.Query(c, fmt.Sprintf(`SELECT n.id, n.category, n.message, n.message_html, n.asset_id, n.location_id, l.parent_id, l.name,
			n.created_at, n.additional_data, nr.notification_id IS NOT NULL, n.recipients
		FROM core.notifications n
		LEFT JOIN asset.locations l ON l.id = n.location_id
		LEFT JOIN core.notification_reads nr ON nr.notification_id = n.id AND nr.user_id = $1
		WHERE %s ORDER BY n.created_at DESC, n.id DESC LIMIT $%d OFFSET $%d`, strings.Join(where, " AND "), len(args)-1, len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type item struct {
		m     map[string]any
		locID *int64
	}
	var items []item
	for rows.Next() {
		var id int64
		var category, message string
		var html, locName *string
		var assetID, locID, parentID *int64
		var at time.Time
		var additional []byte
		var isRead bool
		var recipients []string
		if err := rows.Scan(&id, &category, &message, &html, &assetID, &locID, &parentID, &locName, &at, &additional, &isRead, &recipients); err != nil {
			return nil, err
		}
		var add *string
		if len(additional) > 0 && string(additional) != "null" {
			str := string(additional)
			add = &str
		}
		items = append(items, item{locID: locID, m: map[string]any{
			"id": id, "category": category, "message": message, "message_html": html, "asset_id": assetID,
			"location_id": locID, "parent_id": parentID, "location_name": locName, "timestamp": httpx.TS(at),
			"additional_data": add, "is_read": isRead, "recipients": emptyIfNil(recipients),
		}})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	extend := int64(len(items)) > limit
	if extend {
		items = items[:limit]
	}
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		anc := []int64{}
		if it.locID != nil {
			anc = tree.ancestors(*it.locID)
		}
		it.m["ancestors"] = anc
		out = append(out, it.m)
	}
	return map[string]any{"is_extend": extend, "notifications": out}, nil
}

func (s *Server) notifUnreadCount(c *Ctx) (any, error) {
	cond, args, err := s.notifVisibility(c, 2)
	if err != nil {
		return nil, err
	}
	args = append([]any{c.User.ID}, args...)
	var n int64
	err = s.DB.QueryRow(c, `SELECT count(*) FROM core.notifications n WHERE `+cond+`
		AND NOT EXISTS (SELECT 1 FROM core.notification_reads nr WHERE nr.notification_id = n.id AND nr.user_id = $1)`, args...).Scan(&n)
	return n, err
}

func (s *Server) notifSetRead(c *Ctx) (any, error) {
	id := c.F.Int("notification_id", 0)
	var err error
	if c.F.Bool("is_read", true) {
		_, err = s.DB.Exec(c, `INSERT INTO core.notification_reads (notification_id, user_id)
			SELECT id, $2 FROM core.notifications WHERE id = $1 ON CONFLICT DO NOTHING`, id, c.User.ID)
	} else {
		_, err = s.DB.Exec(c, `DELETE FROM core.notification_reads WHERE notification_id = $1 AND user_id = $2`, id, c.User.ID)
	}
	if err != nil {
		return nil, err
	}
	return "success", nil
}

func (s *Server) notifReadAll(c *Ctx) (any, error) {
	cond, args, err := s.notifVisibility(c, 2)
	if err != nil {
		return nil, err
	}
	args = append([]any{c.User.ID}, args...)
	if _, err := s.DB.Exec(c, `INSERT INTO core.notification_reads (notification_id, user_id)
		SELECT n.id, $1 FROM core.notifications n WHERE `+cond+` ON CONFLICT DO NOTHING`, args...); err != nil {
		return nil, err
	}
	return "success", nil
}

// ---------------------------------------------------------------------------
// Notification manager

func (s *Server) nmEmailDetails(c *Ctx) (any, error) {
	if err := c.Require("notificationmanager", "read"); err != nil {
		return nil, err
	}
	alias := c.F.StrTrim("alias")
	var id, port, interval int
	var name, email, sender, pass, host string
	var secure, active bool
	err := s.DB.QueryRow(c, `SELECT notification_id, notification_name, sender_email, sender_name, sender_password, smtp_host, smtp_port,
		is_secure, interval_minutes, is_active FROM core.email_configs WHERE alias = $1`, alias).
		Scan(&id, &name, &email, &sender, &pass, &host, &port, &secure, &interval, &active)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("Notification not found")
		}
		return nil, err
	}
	return map[string]any{"alias": alias, "notification_id": id, "is_secure": secure, "notification_name": name, "sender_email": email,
		"sender_name": sender, "sender_password": pass, "smtp_host": host, "smtp_port": port, "timer_id": id,
		"is_timer_active": active, "recurring_type": "minute", "recurring_value": interval}, nil
}

func (s *Server) nmRegisterEmail(c *Ctx) (any, error) {
	if err := c.Require("notificationmanager", "update"); err != nil {
		return nil, err
	}
	alias := c.F.StrTrim("alias")
	if alias == "" {
		alias = "epm_email_notification"
	}
	interval := c.F.Int("interval", 5)
	port := c.F.Int("smtp_port", 587)
	active := c.F.Bool("is_active", false)
	host, email := c.F.StrTrim("smtp_host"), c.F.StrTrim("sender_email")
	if interval < 1 {
		return nil, httpx.BadRequest("err_input_valid_number")
	}
	if interval > 1440 {
		return nil, httpx.BadRequest("err_interval_cannot_exceed_24_hours")
	}
	if port < 1 || port > 65535 {
		return nil, httpx.BadRequest("err_please_input_valid_port")
	}
	if active && host == "" {
		return nil, httpx.BadRequest("err_smtp_host_is_required")
	}
	if active && email == "" {
		return nil, httpx.BadRequest("err_email_sender_is_required")
	}
	name := c.F.StrTrim("name")
	if name == "" {
		name = "EPM Email Notification"
	}
	sender := c.F.StrTrim("sender_name")
	if sender == "" {
		sender = "EPM Notification"
	}
	_, err := s.DB.Exec(c, `INSERT INTO core.email_configs AS e (alias, notification_name, sender_email, sender_name, sender_password, smtp_host,
			smtp_port, is_secure, interval_minutes, is_active, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, now())
		ON CONFLICT (alias) DO UPDATE SET notification_name = EXCLUDED.notification_name, sender_email = EXCLUDED.sender_email,
			sender_name = EXCLUDED.sender_name, sender_password = EXCLUDED.sender_password, smtp_host = EXCLUDED.smtp_host,
			smtp_port = EXCLUDED.smtp_port, is_secure = EXCLUDED.is_secure, interval_minutes = EXCLUDED.interval_minutes,
			is_active = EXCLUDED.is_active, updated_at = now(),
			last_notification_id = CASE WHEN e.is_active AND EXCLUDED.is_active THEN e.last_notification_id ELSE 0 END`,
		alias, name, email, sender, c.F.Str("sender_password"), host, port, c.F.Bool("is_secure", false), interval, active)
	if err != nil {
		return nil, err
	}
	return "success_update_mailing_configuration", nil
}

func (s *Server) nmUserGroups(c *Ctx) (any, error) {
	if err := c.Require("notificationmanager", "read"); err != nil {
		return nil, err
	}
	alias := c.F.StrTrim("notification_alias")
	args := []any{alias, ilike(c.F.Str("search"))}
	filter := ""
	if loc := c.F.IntPtr("location_id"); loc != nil && *loc > 0 {
		tree, err := s.loadLocations(c)
		if err != nil {
			return nil, err
		}
		related := tree.ancestors(*loc)
		for id := range tree.descendants([]int64{*loc}) {
			related = append(related, id)
		}
		args = append(args, uniqueInt64(related))
		filter = ` AND EXISTS (SELECT 1 FROM core.user_group_locations x WHERE x.user_group_id = g.id AND x.location_id = ANY($3))`
	}
	rows, err := s.DB.Query(c, `SELECT g.alias, g.name,
			(SELECT count(*) FROM core.email_group_categories e JOIN core.notification_categories nc ON nc.id = e.category_id
				WHERE e.user_group_id = g.id AND e.notification_alias = $1 AND nc.is_selectable),
			COALESCE((SELECT string_agg(l.name, ', ' ORDER BY l.name) FROM core.user_group_locations x JOIN asset.locations l ON l.id = x.location_id
				WHERE x.user_group_id = g.id), ''),
			(SELECT count(*) FROM core.users u WHERE u.user_group_id = g.id AND NOT u.is_deleted)
		FROM core.user_groups g WHERE (g.name ILIKE $2 OR g.alias ILIKE $2)`+filter+` ORDER BY lower(g.name)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var a, name, locs string
		var cats, users int64
		if err := rows.Scan(&a, &name, &cats, &locs, &users); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"alias": a, "name": name, "category_count": cats, "locations": locs, "user_count": users})
	}
	return out, rows.Err()
}

func (s *Server) nmLocationOptions(c *Ctx) (any, error) {
	if err := c.Require("notificationmanager", "read"); err != nil {
		return nil, err
	}
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT id FROM asset.locations`)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	type opt struct {
		id   int64
		path string
	}
	opts := make([]opt, 0, len(ids))
	for _, id := range ids {
		opts = append(opts, opt{id, tree.path(id, " / ")})
	}
	sort.SliceStable(opts, func(i, j int) bool { return strings.ToLower(opts[i].path) < strings.ToLower(opts[j].path) })
	out := make([]map[string]any, 0, len(opts))
	for _, o := range opts {
		out = append(out, map[string]any{"value": fmt.Sprint(o.id), "label": o.path})
	}
	return out, nil
}

func (s *Server) nmCategories(c *Ctx) (any, error) {
	if err := c.Require("notificationmanager", "read"); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT id, name FROM core.notification_categories WHERE is_selectable AND (name ILIKE $1 OR alias ILIKE $1) ORDER BY id`,
		ilike(c.F.Str("search")))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "category": name})
	}
	return out, rows.Err()
}

func (s *Server) groupIDByAlias(c *Ctx, q db.Querier, alias string) (int64, error) {
	var id int64
	if err := q.QueryRow(c, `SELECT id FROM core.user_groups WHERE lower(alias) = lower($1)`, strings.TrimSpace(alias)).Scan(&id); err != nil {
		if db.IsNoRows(err) {
			return 0, httpx.NotFound("User group not found")
		}
		return 0, err
	}
	return id, nil
}

func (s *Server) nmAssignedCategories(c *Ctx) (any, error) {
	if err := c.Require("notificationmanager", "read"); err != nil {
		return nil, err
	}
	gid, err := s.groupIDByAlias(c, s.DB, c.F.Str("ug_alias"))
	if err != nil {
		if he, ok := err.(*httpx.Error); ok && he.Status == 404 {
			return []map[string]any{}, nil
		}
		return nil, err
	}
	ids, err := db.Int64s(c, s.DB, `SELECT category_id FROM core.email_group_categories WHERE notification_alias = $1 AND user_group_id = $2 ORDER BY category_id`,
		c.F.StrTrim("notification_alias"), gid)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]any{"id": id})
	}
	return out, nil
}

func (s *Server) nmRegisterGroup(c *Ctx) (any, error) {
	if err := c.Require("notificationmanager", "update"); err != nil {
		return nil, err
	}
	var input []struct {
		CategoryID int64  `json:"category_id"`
		NotifAlias string `json:"notif_alias"`
		UGAlias    string `json:"ug_alias"`
	}
	if err := json.Unmarshal([]byte(c.F.Str("input")), &input); err != nil {
		return nil, httpx.BadRequest("Invalid input")
	}
	ugAlias, notifAlias := c.F.StrTrim("ug_alias"), c.F.StrTrim("notification_alias")
	if len(input) > 0 {
		ugAlias, notifAlias = input[0].UGAlias, input[0].NotifAlias
	}
	if ugAlias == "" {
		// The editor sends no group when every category is deselected; the
		// page URL (Referer) still carries it.
		if ref, err := url.Parse(c.R.Referer()); err == nil {
			ugAlias = ref.Query().Get("ug_alias")
		}
	}
	if ugAlias == "" {
		return nil, httpx.BadRequest("Select at least one category")
	}
	if notifAlias == "" {
		notifAlias = "epm_email_notification"
	}
	err := s.DB.Tx(c, func(tx pgx.Tx) error {
		gid, err := s.groupIDByAlias(c, tx, ugAlias)
		if err != nil {
			return err
		}
		ids := []int64{}
		for _, in := range input {
			if !strings.EqualFold(in.UGAlias, ugAlias) || in.NotifAlias != notifAlias {
				return httpx.BadRequest("Invalid input")
			}
			ids = append(ids, in.CategoryID)
		}
		ids = uniqueInt64(ids)
		var valid int
		if err := tx.QueryRow(c, `SELECT count(*) FROM core.notification_categories WHERE id = ANY($1) AND is_selectable`, ids).Scan(&valid); err != nil {
			return err
		}
		if valid != len(ids) {
			return httpx.BadRequest("Invalid notification category")
		}
		if _, err := tx.Exec(c, `DELETE FROM core.email_group_categories WHERE notification_alias = $1 AND user_group_id = $2`, notifAlias, gid); err != nil {
			return err
		}
		_, err = tx.Exec(c, `INSERT INTO core.email_group_categories (notification_alias, user_group_id, category_id)
			SELECT $1, $2, unnest($3::int[])`, notifAlias, gid, ids)
		if db.IsForeignKeyViolation(err) {
			return httpx.BadRequest("Notification not found")
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return "success_assign_category", nil
}

func (s *Server) nmUsers(c *Ctx) (any, error) {
	if err := c.Require("notificationmanager", "read"); err != nil {
		return nil, err
	}
	gid, err := s.groupIDByAlias(c, s.DB, c.F.Str("ug_alias"))
	if err != nil {
		if he, ok := err.(*httpx.Error); ok && he.Status == 404 {
			return []map[string]any{}, nil
		}
		return nil, err
	}
	dir := "ASC"
	if !c.F.Bool("ascending", true) {
		dir = "DESC"
	}
	rows, err := s.DB.Query(c, `SELECT username, CASE WHEN full_name = '' THEN username ELSE full_name END AS label FROM core.users
		WHERE user_group_id = $1 AND NOT is_deleted AND (username ILIKE $2 OR full_name ILIKE $2 OR email ILIKE $2)
		ORDER BY lower(CASE WHEN full_name = '' THEN username ELSE full_name END) `+dir+`, id`, gid, ilike(c.F.Str("search")))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var username, label string
		if err := rows.Scan(&username, &label); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": username, "user": label})
	}
	return out, rows.Err()
}

func (s *Server) nmUnsubscribed(c *Ctx) (any, error) {
	if err := c.Require("notificationmanager", "read"); err != nil {
		return nil, err
	}
	gid, err := s.groupIDByAlias(c, s.DB, c.F.Str("ug_alias"))
	if err != nil {
		if he, ok := err.(*httpx.Error); ok && he.Status == 404 {
			return []map[string]any{}, nil
		}
		return nil, err
	}
	names, err := db.Strings(c, s.DB, `SELECT u.username FROM core.users u JOIN core.email_unsubscribed_users x ON x.user_id = u.id
		WHERE u.user_group_id = $1 AND NOT u.is_deleted AND x.type_id = $2 ORDER BY lower(u.username)`, gid, c.F.Int("type_id", 1))
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"username": n})
	}
	return out, nil
}

func (s *Server) nmSubmitUnsubscribed(c *Ctx) (any, error) {
	if err := c.Require("notificationmanager", "update"); err != nil {
		return nil, err
	}
	type entry struct {
		Username string `json:"username"`
	}
	var selected, unselected []entry
	if raw := c.F.Str("selected_usernames"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &selected); err != nil {
			return nil, httpx.BadRequest("Invalid input")
		}
	}
	if raw := c.F.Str("unselected_usernames"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &unselected); err != nil {
			return nil, httpx.BadRequest("Invalid input")
		}
	}
	typeID := c.F.Int("type_id", 1)
	names := func(list []entry) []string {
		out := []string{}
		for _, e := range list {
			if n := strings.ToLower(strings.TrimSpace(e.Username)); n != "" {
				out = append(out, n)
			}
		}
		return out
	}
	err := s.DB.Tx(c, func(tx pgx.Tx) error {
		if _, err := tx.Exec(c, `DELETE FROM core.email_unsubscribed_users x USING core.users u
			WHERE x.user_id = u.id AND x.type_id = $2 AND lower(u.username) = ANY($1)`, names(selected), typeID); err != nil {
			return err
		}
		_, err := tx.Exec(c, `INSERT INTO core.email_unsubscribed_users (user_id, type_id)
			SELECT id, $2 FROM core.users WHERE NOT is_deleted AND lower(username) = ANY($1) ON CONFLICT DO NOTHING`, names(unselected), typeID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return "success_update_notified_users", nil
}
