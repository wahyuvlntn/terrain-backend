package api

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"afdd/internal/auth"
	"afdd/internal/db"
	"afdd/internal/httpx"
	"afdd/internal/storage"
)

var usernameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)
var emailRe = regexp.MustCompile(`^[\w\-.+]+@([\w-]+\.)+[\w-]{2,}$`)

func (s *Server) registerUser() {
	s.handle("user/getuserdetails", AccessUser, s.userGetDetails)
	s.handle("user/getuserprofiledetail", AccessUser, s.userGetProfile)
	s.handle("user/changepassword", AccessUser, s.userChangePassword)
	s.handle("user/verifypassword", AccessUser, s.userVerifyPassword)
	s.handle("user/updateprofile", AccessUser, s.userUpdateProfile)
	s.handle("auth/checkpermission", AccessUser, s.authCheckPermission)

	s.handle("user_management/getusergroups", AccessAdmin, s.umGetUserGroups)
	s.handle("user_management/getusergroupdetails", AccessAdmin, s.umGetUserGroupDetails)
	s.handle("user_management/addusergroup", AccessAdmin, s.umAddUserGroup)
	s.handle("user_management/updateusergroup", AccessAdmin, s.umUpdateUserGroup)
	s.handle("user_management/deleteusergroup", AccessAdmin, s.umDeleteUserGroup)
	s.handle("user_management/addusergrouppermission", AccessAdmin, s.umSetGroupPermissions)
	s.handle("user_management/getusergrouppermission", AccessAdmin, s.umGetGroupPermissions)
	s.handle("user_management/getuserpermission", AccessAdmin, s.umGetUserPermissions)
	s.handle("user_management/getusersbyusergroup", AccessAdmin, s.umGetUsersByGroup)
	s.handle("user_management/getuserdetails", AccessAdmin, s.umGetUserDetails)
	s.handle("user_management/adduser", AccessAdmin, s.umAddUser)
	s.handle("user_management/updateuser", AccessAdmin, s.umUpdateUser)
	s.handle("user_management/deleteuser", AccessAdmin, s.umDeleteUser)
	s.handle("user_management/getlocationtree", AccessAdmin, s.umGetLocationTree)
}

func (s *Server) userGetDetails(c *Ctx) (any, error) {
	u := c.User
	perms, err := s.Auth.PermissionList(c, u)
	if err != nil {
		return nil, err
	}
	var locs []int64
	if u.IsAdmin() {
		locs, err = db.Int64s(c, s.DB, `SELECT id FROM asset.locations ORDER BY id`)
		if err != nil {
			return nil, err
		}
	} else {
		scope, err := c.Scope()
		if err != nil {
			return nil, err
		}
		locs = scope.LocationIDs()
		sort.Slice(locs, func(i, j int) bool { return locs[i] < locs[j] })
	}
	return map[string]any{
		"id":                u.ID,
		"username":          u.Username,
		"full_name":         u.FullName,
		"employee_id":       u.EmployeeID,
		"photo":             photoURL(u.Photo),
		"role":              u.Role,
		"assignedLocations": emptyIfNil(locs),
		"permissions":       perms,
	}, nil
}

func (s *Server) groupLocationNames(ctx context.Context, groupID *int64) (string, error) {
	if groupID == nil {
		return "", nil
	}
	names, err := db.Strings(ctx, s.DB, `SELECT l.name FROM core.user_group_locations ugl JOIN asset.locations l ON l.id = ugl.location_id
		WHERE ugl.user_group_id = $1 ORDER BY l.name`, *groupID)
	if err != nil {
		return "", err
	}
	return strings.Join(names, ", "), nil
}

func (s *Server) userGetProfile(c *Ctx) (any, error) {
	u := c.User
	assigned := "All Locations"
	if !u.IsAdmin() {
		names, err := s.groupLocationNames(c, u.UserGroupID)
		if err != nil {
			return nil, err
		}
		assigned = names
		if assigned == "" {
			assigned = "-"
		}
	}
	var photo *string
	if p := photoURL(u.Photo); p != "" {
		photo = &p
	}
	return map[string]any{
		"assignedLocation": assigned,
		"employeeID":       u.EmployeeID,
		"name":             u.FullName,
		"photoURL":         photo,
		"username":         u.Username,
		"role":             u.Role,
	}, nil
}

func (s *Server) currentPasswordHash(ctx context.Context, userID int64) (string, error) {
	var hash string
	err := s.DB.QueryRow(ctx, `SELECT password_hash FROM core.users WHERE id = $1`, userID).Scan(&hash)
	return hash, err
}

func (s *Server) userVerifyPassword(c *Ctx) (any, error) {
	hash, err := s.currentPasswordHash(c, c.User.ID)
	if err != nil {
		return nil, err
	}
	if !auth.CheckPassword(hash, c.F.Str("old_password")) {
		return nil, httpx.BadRequest("Incorrect password")
	}
	return "ok", nil
}

func (s *Server) userChangePassword(c *Ctx) (any, error) {
	oldPw := c.F.Str("old_password")
	newPw := c.F.Str("new_password")
	hash, err := s.currentPasswordHash(c, c.User.ID)
	if err != nil {
		return nil, err
	}
	if !auth.CheckPassword(hash, oldPw) {
		return nil, httpx.BadRequest("Incorrect password")
	}
	if key := auth.ValidatePasswordPolicy(newPw); key != "" {
		return nil, httpx.BadRequest(key)
	}
	newHash, err := auth.HashPassword(newPw)
	if err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(c, `UPDATE core.users SET password_hash = $2, updated_at = now() WHERE id = $1`, c.User.ID, newHash); err != nil {
		return nil, err
	}
	return "Password successfully changed", nil
}

func (s *Server) userUpdateProfile(c *Ctx) (any, error) {
	name := c.F.StrTrim("name")
	if name == "" {
		return nil, httpx.BadRequest("err_name_is_required")
	}
	var photo *string
	if fh := c.F.File("photo"); fh != nil {
		saved, err := s.Store.SaveMultipart(c, "users", fh, storage.AllowedImageTypes)
		if err != nil {
			return nil, fail(err)
		}
		photo = &saved.URL
	}
	var old *string
	if err := s.DB.QueryRow(c, `SELECT photo FROM core.users WHERE id = $1`, c.User.ID).Scan(&old); err != nil {
		return nil, err
	}
	if photo != nil {
		if _, err := s.DB.Exec(c, `UPDATE core.users SET full_name = $2, photo = $3, updated_at = now() WHERE id = $1`, c.User.ID, name, *photo); err != nil {
			return nil, err
		}
		if old != nil {
			s.Store.DeleteURL(c, *old)
		}
	} else if _, err := s.DB.Exec(c, `UPDATE core.users SET full_name = $2, updated_at = now() WHERE id = $1`, c.User.ID, name); err != nil {
		return nil, err
	}
	s.Auth.InvalidateUser(c.User.ID)
	return "update_user_detail_success", nil
}

func (s *Server) authCheckPermission(c *Ctx) (any, error) {
	return c.User.Can(c.F.StrTrim("permission_group"), c.F.StrTrim("permission")), nil
}

// ---------------------------------------------------------------------------
// User groups
// ---------------------------------------------------------------------------

type groupLoc struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

func (s *Server) groupLocations(ctx context.Context, groupID int64, tree *locTree) ([]groupLoc, error) {
	ids, err := db.Int64s(ctx, s.DB, `SELECT location_id FROM core.user_group_locations WHERE user_group_id = $1`, groupID)
	if err != nil {
		return nil, err
	}
	out := []groupLoc{}
	for _, id := range ids {
		out = append(out, groupLoc{ID: strconv.FormatInt(id, 10), Label: tree.name(id)})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Label) < strings.ToLower(out[j].Label) })
	return out, nil
}

func (s *Server) umGetUserGroups(c *Ctx) (any, error) {
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	search := c.F.StrTrim("search")
	rows, err := s.DB.Query(c, `SELECT id, alias, name, description FROM core.user_groups
		WHERE $1 = '' OR name ILIKE $2 OR alias ILIKE $2 ORDER BY lower(name)`, search, ilike(search))
	if err != nil {
		return nil, err
	}
	type item struct {
		Alias       string     `json:"alias"`
		Description string     `json:"description"`
		Name        string     `json:"name"`
		Locations   []groupLoc `json:"locations"`
		id          int64
	}
	var items []*item
	for rows.Next() {
		it := &item{}
		if err := rows.Scan(&it.id, &it.Alias, &it.Name, &it.Description); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, it)
	}
	rows.Close()
	for _, it := range items {
		if it.Locations, err = s.groupLocations(c, it.id, tree); err != nil {
			return nil, err
		}
	}
	if asc := c.F.BoolPtr("ascending_ug"); asc != nil {
		sort.SliceStable(items, func(i, j int) bool {
			a, b := strings.ToLower(items[i].Name), strings.ToLower(items[j].Name)
			if *asc {
				return a < b
			}
			return a > b
		})
	} else if asc := c.F.BoolPtr("ascending_loc"); asc != nil {
		label := func(it *item) string {
			var ls []string
			for _, l := range it.Locations {
				ls = append(ls, strings.ToLower(l.Label))
			}
			return strings.Join(ls, ",")
		}
		sort.SliceStable(items, func(i, j int) bool {
			if *asc {
				return label(items[i]) < label(items[j])
			}
			return label(items[i]) > label(items[j])
		})
	}
	return emptyIfNil(items), nil
}

func (s *Server) groupByAlias(ctx context.Context, q db.Querier, alias string) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `SELECT id FROM core.user_groups WHERE lower(alias) = lower($1)`, strings.TrimSpace(alias)).Scan(&id)
	if err != nil {
		if db.IsNoRows(err) {
			return 0, httpx.NotFound("User group not found")
		}
		return 0, err
	}
	return id, nil
}

func (s *Server) umGetUserGroupDetails(c *Ctx) (any, error) {
	id, err := s.groupByAlias(c, s.DB, c.F.Str("alias"))
	if err != nil {
		return nil, err
	}
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	var alias, name, desc string
	if err := s.DB.QueryRow(c, `SELECT alias, name, description FROM core.user_groups WHERE id = $1`, id).Scan(&alias, &name, &desc); err != nil {
		return nil, err
	}
	locs, err := s.groupLocations(c, id, tree)
	if err != nil {
		return nil, err
	}
	return map[string]any{"alias": alias, "name": name, "description": desc, "locations": locs}, nil
}

func parseGroupLocations(raw string) []int64 {
	var items []struct {
		ID any `json:"id"`
	}
	if raw == "" {
		return nil
	}
	if err := jsonUnmarshal(raw, &items); err != nil {
		return httpx.ParseIDList(raw)
	}
	var out []int64
	for _, it := range items {
		switch v := it.ID.(type) {
		case string:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				out = append(out, n)
			}
		case float64:
			out = append(out, int64(v))
		}
	}
	return uniqueInt64(out)
}

func (s *Server) saveGroup(c *Ctx, create bool) (any, error) {
	alias := c.F.StrTrim("alias")
	name := c.F.StrTrim("name")
	desc := c.F.Str("description")
	if name == "" {
		return nil, httpx.BadRequest("err_name_is_required")
	}
	if alias == "" {
		return nil, httpx.BadRequest("err_alias_is_required")
	}
	if len([]rune(desc)) > 160 {
		return nil, httpx.BadRequest("Description is too long")
	}
	locs := parseGroupLocations(c.F.Str("locations"))
	err := s.DB.Tx(c, func(tx pgx.Tx) error {
		var id int64
		if create {
			if err := tx.QueryRow(c, `INSERT INTO core.user_groups (alias, name, description) VALUES ($1, $2, $3) RETURNING id`, alias, name, desc).Scan(&id); err != nil {
				if db.IsUniqueViolation(err) {
					return httpx.BadRequest("User group with the same ID already exists")
				}
				return err
			}
		} else {
			var err error
			if id, err = s.groupByAlias(c, tx, alias); err != nil {
				return err
			}
			if _, err := tx.Exec(c, `UPDATE core.user_groups SET name = $2, description = $3, updated_at = now() WHERE id = $1`, id, name, desc); err != nil {
				return err
			}
			if _, err := tx.Exec(c, `DELETE FROM core.user_group_locations WHERE user_group_id = $1`, id); err != nil {
				return err
			}
		}
		for _, l := range locs {
			if _, err := tx.Exec(c, `INSERT INTO core.user_group_locations (user_group_id, location_id)
				SELECT $1, $2 WHERE EXISTS (SELECT 1 FROM asset.locations WHERE id = $2) ON CONFLICT DO NOTHING`, id, l); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.Auth.InvalidateAll()
	if create {
		return map[string]any{"message": "success_add_user_group"}, nil
	}
	return map[string]any{"message": "success_update_user_group"}, nil
}

func (s *Server) umAddUserGroup(c *Ctx) (any, error)    { return s.saveGroup(c, true) }
func (s *Server) umUpdateUserGroup(c *Ctx) (any, error) { return s.saveGroup(c, false) }

func (s *Server) umDeleteUserGroup(c *Ctx) (any, error) {
	id, err := s.groupByAlias(c, s.DB, c.F.Str("alias"))
	if err != nil {
		return nil, err
	}
	var n int
	if err := s.DB.QueryRow(c, `SELECT count(*) FROM core.users WHERE user_group_id = $1 AND NOT is_deleted`, id).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, httpx.BadRequest("Remove all users from this user group first")
	}
	if _, err := s.DB.Exec(c, `UPDATE core.users SET user_group_id = NULL WHERE user_group_id = $1`, id); err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(c, `DELETE FROM core.user_groups WHERE id = $1`, id); err != nil {
		return nil, err
	}
	s.Auth.InvalidateAll()
	return map[string]any{"message": "success_delete_user_group"}, nil
}

type permRow struct {
	Permission           string `json:"permission"`
	PermissionAlias      string `json:"permission_alias"`
	PermissionGroup      string `json:"permission_group"`
	PermissionGroupAlias string `json:"permission_group_alias"`
	Value                bool   `json:"value"`
}

func (s *Server) permissionRows(ctx context.Context, groupID *int64, all bool) ([]permRow, error) {
	var gid int64 = -1
	if groupID != nil {
		gid = *groupID
	}
	rows, err := s.DB.Query(ctx, `SELECT p.name, p.alias, g.name, g.alias, COALESCE(ugp.value, FALSE)
		FROM core.permissions p JOIN core.permission_groups g ON g.id = p.permission_group_id
		LEFT JOIN core.user_group_permissions ugp ON ugp.permission_id = p.id AND ugp.user_group_id = $1
		ORDER BY g.sort_order, p.id`, gid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []permRow{}
	for rows.Next() {
		var r permRow
		if err := rows.Scan(&r.Permission, &r.PermissionAlias, &r.PermissionGroup, &r.PermissionGroupAlias, &r.Value); err != nil {
			return nil, err
		}
		if all {
			r.Value = true
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Server) umGetGroupPermissions(c *Ctx) (any, error) {
	id, err := s.groupByAlias(c, s.DB, c.F.Str("alias"))
	if err != nil {
		return nil, err
	}
	return s.permissionRows(c, &id, false)
}

func (s *Server) umSetGroupPermissions(c *Ctx) (any, error) {
	id, err := s.groupByAlias(c, s.DB, c.F.Str("alias"))
	if err != nil {
		return nil, err
	}
	var items []struct {
		Group string `json:"permission_group_alias"`
		Alias string `json:"permission_alias"`
		Value bool   `json:"value"`
	}
	if _, err := c.F.JSON("permissions", &items); err != nil {
		return nil, httpx.BadRequest("Invalid permissions")
	}
	err = s.DB.Tx(c, func(tx pgx.Tx) error {
		for _, it := range items {
			if _, err := tx.Exec(c, `INSERT INTO core.user_group_permissions (user_group_id, permission_id, value)
				SELECT $1, p.id, $4 FROM core.permissions p JOIN core.permission_groups g ON g.id = p.permission_group_id
				WHERE g.alias = $2 AND p.alias = $3
				ON CONFLICT (user_group_id, permission_id) DO UPDATE SET value = EXCLUDED.value`, id, it.Group, it.Alias, it.Value); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.Auth.InvalidateAll()
	return map[string]any{"message": "success_update_user_group"}, nil
}

type userRecord struct {
	ID         int64
	Username   string
	FullName   string
	Email      string
	EmployeeID *string
	Photo      *string
	Role       string
	GroupID    *int64
	GroupAlias *string
}

func (s *Server) userByUsername(ctx context.Context, q db.Querier, username string) (*userRecord, error) {
	u := &userRecord{}
	err := q.QueryRow(ctx, `SELECT u.id, u.username, u.full_name, u.email, u.employee_id, u.photo, u.role, u.user_group_id, g.alias
		FROM core.users u LEFT JOIN core.user_groups g ON g.id = u.user_group_id
		WHERE lower(u.username) = lower($1) AND NOT u.is_deleted`, strings.TrimSpace(username)).
		Scan(&u.ID, &u.Username, &u.FullName, &u.Email, &u.EmployeeID, &u.Photo, &u.Role, &u.GroupID, &u.GroupAlias)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, httpx.NotFound("User not found")
		}
		return nil, err
	}
	return u, nil
}

func (s *Server) umGetUserPermissions(c *Ctx) (any, error) {
	u, err := s.userByUsername(c, s.DB, c.F.Str("username"))
	if err != nil {
		return nil, err
	}
	return s.permissionRows(c, u.GroupID, u.Role != auth.RoleBasic)
}

func (s *Server) umGetUsersByGroup(c *Ctx) (any, error) {
	id, err := s.groupByAlias(c, s.DB, c.F.Str("user_group_alias"))
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(c, `SELECT full_name, photo, username FROM core.users WHERE user_group_id = $1 AND NOT is_deleted ORDER BY lower(full_name), username`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name, username string
		var photo *string
		if err := rows.Scan(&name, &photo, &username); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"name": name, "photo": photoURL(photo), "username": username})
	}
	return out, rows.Err()
}

func (s *Server) umGetUserDetails(c *Ctx) (any, error) {
	u, err := s.userByUsername(c, s.DB, c.F.Str("username"))
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"name": u.FullName, "photo": photoURL(u.Photo), "username": u.Username, "employee_id": derefStr(u.EmployeeID),
		"email": u.Email, "user_group_alias": derefStr(u.GroupAlias),
	}, nil
}

func (s *Server) validateUserFields(c *Ctx) (username, name, email, employeeID string, err error) {
	username = c.F.StrTrim("username")
	name = c.F.StrTrim("name")
	email = c.F.StrTrim("email")
	employeeID = c.F.StrTrim("user_id")
	switch {
	case username == "":
		err = httpx.BadRequest("err_username_required")
	case !usernameRe.MatchString(username):
		err = httpx.BadRequest("err_username_validation")
	case name == "":
		err = httpx.BadRequest("err_name_is_required")
	case employeeID == "":
		err = httpx.BadRequest("err_user_id_required")
	case !emailRe.MatchString(email):
		err = httpx.BadRequest("Invalid email address")
	}
	return
}

func (s *Server) umAddUser(c *Ctx) (any, error) {
	username, name, email, employeeID, err := s.validateUserFields(c)
	if err != nil {
		return nil, err
	}
	password := c.F.Str("password")
	if password == "" {
		return nil, httpx.BadRequest("err_password_required")
	}
	if key := auth.ValidatePasswordPolicy(password); key != "" {
		return nil, httpx.BadRequest(key)
	}
	groupID, err := s.groupByAlias(c, s.DB, c.F.Str("user_group_alias"))
	if err != nil {
		return nil, httpx.BadRequest("err_user_group_required")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return nil, err
	}
	var photo *string
	if fh := c.F.File("photo"); fh != nil {
		saved, err := s.Store.SaveMultipart(c, "users", fh, storage.AllowedImageTypes)
		if err != nil {
			return nil, fail(err)
		}
		photo = &saved.URL
	}
	var id int64
	err = s.DB.QueryRow(c, `INSERT INTO core.users (username, password_hash, full_name, email, employee_id, photo, role, user_group_id)
		VALUES ($1, $2, $3, $4, $5, $6, 'basic', $7) RETURNING id`, username, hash, name, email, employeeID, photo, groupID).Scan(&id)
	if err != nil {
		if photo != nil {
			s.Store.DeleteURL(c, *photo)
		}
		if db.IsUniqueViolation(err) {
			return nil, httpx.BadRequest("Username already exists")
		}
		return nil, err
	}
	_, _ = s.DB.Exec(c, `INSERT INTO dm.personnel (user_id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
	return map[string]any{"message": "success_add_user"}, nil
}

func (s *Server) umUpdateUser(c *Ctx) (any, error) {
	_, name, email, employeeID, err := s.validateUserFields(c)
	if err != nil {
		return nil, err
	}
	u, err := s.userByUsername(c, s.DB, c.F.Str("username"))
	if err != nil {
		return nil, err
	}
	if u.Role == auth.RoleSuperuser && !c.User.IsSuperuser() {
		return nil, httpx.Forbidden("")
	}
	groupID, err := s.groupByAlias(c, s.DB, c.F.Str("user_group_alias"))
	if err != nil {
		return nil, httpx.BadRequest("err_user_group_required")
	}
	var newHash *string
	if pw := c.F.Str("password"); pw != "" {
		if key := auth.ValidatePasswordPolicy(pw); key != "" {
			return nil, httpx.BadRequest(key)
		}
		// When the editor is changing their own password the old one must match.
		if u.ID == c.User.ID {
			hash, err := s.currentPasswordHash(c, u.ID)
			if err != nil {
				return nil, err
			}
			if !auth.CheckPassword(hash, c.F.Str("old_password")) {
				return nil, httpx.BadRequest("Incorrect password")
			}
		}
		h, err := auth.HashPassword(pw)
		if err != nil {
			return nil, err
		}
		newHash = &h
	}
	var photo *string
	if fh := c.F.File("photo"); fh != nil {
		saved, err := s.Store.SaveMultipart(c, "users", fh, storage.AllowedImageTypes)
		if err != nil {
			return nil, fail(err)
		}
		photo = &saved.URL
	}
	_, err = s.DB.Exec(c, `UPDATE core.users SET full_name = $2, email = $3, employee_id = $4, user_group_id = $5,
		photo = COALESCE($6, photo), password_hash = COALESCE($7, password_hash), updated_at = now() WHERE id = $1`,
		u.ID, name, email, employeeID, groupID, photo, newHash)
	if err != nil {
		return nil, err
	}
	if photo != nil && u.Photo != nil {
		s.Store.DeleteURL(c, *u.Photo)
	}
	if newHash != nil && u.ID != c.User.ID {
		s.Auth.DeleteUserSessions(c, u.ID)
	}
	s.Auth.InvalidateUser(u.ID)
	return map[string]any{"message": "success_update_user"}, nil
}

func (s *Server) umDeleteUser(c *Ctx) (any, error) {
	u, err := s.userByUsername(c, s.DB, c.F.Str("username"))
	if err != nil {
		return nil, err
	}
	if u.ID == c.User.ID {
		return nil, httpx.BadRequest("You cannot delete your own account")
	}
	if u.Role == auth.RoleSuperuser && !c.User.IsSuperuser() {
		return nil, httpx.Forbidden("")
	}
	if _, err := s.DB.Exec(c, `UPDATE core.users SET is_deleted = TRUE, deleted_at = now(), updated_at = now() WHERE id = $1`, u.ID); err != nil {
		return nil, err
	}
	s.Auth.DeleteUserSessions(c, u.ID)
	return map[string]any{"message": "success_delete_user"}, nil
}

type uiTreeNode struct {
	ID       string        `json:"id"`
	Label    string        `json:"label"`
	Children []*uiTreeNode `json:"children,omitempty"`
	Checked  bool          `json:"checked"`
}

func (s *Server) umGetLocationTree(c *Ctx) (any, error) {
	tree, err := s.loadLocations(c)
	if err != nil {
		return nil, err
	}
	search := strings.ToLower(c.F.StrTrim("search_filter"))
	var build func(id int64) *uiTreeNode
	build = func(id int64) *uiTreeNode {
		n := tree.nodes[id]
		node := &uiTreeNode{ID: strconv.FormatInt(id, 10), Label: n.Name}
		for _, ch := range n.Children {
			if cn := build(ch); cn != nil {
				node.Children = append(node.Children, cn)
			}
		}
		if search != "" && !strings.Contains(strings.ToLower(n.Name), search) && len(node.Children) == 0 {
			return nil
		}
		return node
	}
	out := []*uiTreeNode{}
	for _, r := range tree.roots {
		if n := build(r); n != nil {
			out = append(out, n)
		}
	}
	return out, nil
}
