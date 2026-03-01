//go:build windows
// +build windows

package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"golang.org/x/sys/windows"
	"golang.zabbix.com/sdk/plugin"
	"golang.zabbix.com/sdk/plugin/container"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"
)

const (
	pluginName = "ADUserMonitoring"

	keyLockedOutUsers         = "LockedOutUsers"         // existing Zabbix item key (must not change)
	keyDisabledUsers          = "DisabledUsers"          // existing Zabbix item key (must not change)
	keyPasswordExpiringUsers  = "PasswordExpiringUsers"  // new Zabbix item key
	keyUsersAboutToBeDisabled = "UsersAboutToBeDisabled" // new Zabbix item key
)


// --- Data models (JSON) ---

type lockedOutUser struct {
	SamAccountName     string  `json:"sAMAccountName"`
	UserPrincipalName  string  `json:"UserPrincipalName"`
	DistinguishedName  string  `json:"DistinguishedName"`
	Enabled            bool    `json:"Enabled"`
	LockedOut          bool    `json:"LockedOut"`
	LockoutTimeUtc     *string `json:"LockoutTimeUtc"`
	WhenCreatedUtc     *string `json:"WhenCreatedUtc"`
	WhenChangedUtc     *string `json:"WhenChangedUtc"`
	UserAccountControl int     `json:"UserAccountControl"`
}

type disabledUser struct {
	SamAccountName     string  `json:"sAMAccountName"`
	UserPrincipalName  string  `json:"UserPrincipalName"`
	DistinguishedName  string  `json:"DistinguishedName"`
	Enabled            bool    `json:"Enabled"`
	DisabledSinceUtc   *string `json:"DisabledSinceUtc"`
	DaysDisabled       *int    `json:"DaysDisabled"`
	WhenCreatedUtc     *string `json:"WhenCreatedUtc"`
	WhenChangedUtc     *string `json:"WhenChangedUtc"`
	UserAccountControl int     `json:"UserAccountControl"`
}

type passwordExpiringUser struct {
	SamAccountName     string  `json:"sAMAccountName"`
	UserPrincipalName  string  `json:"UserPrincipalName"`
	DistinguishedName  string  `json:"DistinguishedName"`
	Enabled            bool    `json:"Enabled"`
	PasswordExpiresUtc *string `json:"PasswordExpiresUtc"`
	DaysToExpire       *int    `json:"DaysToExpire"`
	UserAccountControl int     `json:"UserAccountControl"`
}

type aboutToBeDisabledUser struct {
	SamAccountName     string  `json:"sAMAccountName"`
	UserPrincipalName  string  `json:"UserPrincipalName"`
	DistinguishedName  string  `json:"DistinguishedName"`
	Enabled            bool    `json:"Enabled"`
	AccountExpiresUtc  *string `json:"AccountExpiresUtc"`
	DaysToDisable      *int    `json:"DaysToDisable"`
	UserAccountControl int     `json:"UserAccountControl"`
}

// --- Zabbix plugin implementation ---

type impl struct {
	plugin.Base
}

var pluginImpl impl

func (p *impl) logf(format string, args ...interface{}) {
	if p.Logger != nil {
		p.Infof(format, args...)
	}
}

func jsonStringOrEmptyArray(p *impl, ctx string, v interface{}, err error) (string, error) {
	if err != nil {
		p.logf("[%s] %v", ctx, err)
		return "[]", nil
	}
	b, mErr := json.Marshal(v)
	if mErr != nil {
		p.logf("[%s] JSON marshal error: %v", ctx, mErr)
		return "[]", nil
	}
	return string(b), nil
}

func parseSearchBases(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	// Support multiple OUs/containers without conflicting with DN commas.
	// Allowed delimiters: ';' or '|' or newlines.
	repl := strings.NewReplacer("\r\n", ";", "\n", ";", "|", ";")
	normalized := repl.Replace(raw)

	parts := strings.Split(normalized, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}


func (p *impl) Export(key string, params []string, _ plugin.ContextProvider) (interface{}, error) {
	switch key {
	case keyLockedOutUsers:
		// Params:
		// LockedOutUsers[searchBases,server]
		searchBasesRaw := ""
		server := ""
		if len(params) >= 1 {
			searchBasesRaw = strings.TrimSpace(params[0])
		}
		if len(params) >= 2 {
			server = strings.TrimSpace(params[1])
		}
		searchBases := parseSearchBases(searchBasesRaw)

		users, err := getLockedOutUsers(searchBases, server)
		out, _ := jsonStringOrEmptyArray(p, keyLockedOutUsers, users, err)
		return out, nil

	case keyDisabledUsers:
		// Params:
		// DisabledUsers[days,searchBases,server]
		// (keeps the Zabbix key unchanged if you don't use params)
		days := 30
		searchBasesRaw := ""
		server := ""

		if len(params) >= 1 && strings.TrimSpace(params[0]) != "" {
			if v, err := strconv.Atoi(strings.TrimSpace(params[0])); err == nil && v >= 0 {
				days = v
			}
		}
		if len(params) >= 2 {
			searchBasesRaw = strings.TrimSpace(params[1])
		}
		if len(params) >= 3 {
			server = strings.TrimSpace(params[2])
		}
		searchBases := parseSearchBases(searchBasesRaw)

		users, err := getDisabledUsers(days, searchBases, server)
		out, _ := jsonStringOrEmptyArray(p, keyDisabledUsers, users, err)
		return out, nil

	case keyPasswordExpiringUsers:
		// Params:
		// PasswordExpiringUsers[days,searchBases,server]
		// days = how many days ahead to include (about to expire)
		days := 7
		searchBasesRaw := ""
		server := ""

		if len(params) >= 1 && strings.TrimSpace(params[0]) != "" {
			if v, err := strconv.Atoi(strings.TrimSpace(params[0])); err == nil && v >= 0 {
				days = v
			}
		}
		if len(params) >= 2 {
			searchBasesRaw = strings.TrimSpace(params[1])
		}
		if len(params) >= 3 {
			server = strings.TrimSpace(params[2])
		}
		searchBases := parseSearchBases(searchBasesRaw)

		users, err := getPasswordExpiringUsers(days, searchBases, server)
		out, _ := jsonStringOrEmptyArray(p, keyPasswordExpiringUsers, users, err)
		return out, nil

	case keyUsersAboutToBeDisabled:
		// Params:
		// UsersAboutToBeDisabled[days,searchBases,server]
		// days = how many days ahead to include (accountExpires)
		days := 7
		searchBasesRaw := ""
		server := ""

		if len(params) >= 1 && strings.TrimSpace(params[0]) != "" {
			if v, err := strconv.Atoi(strings.TrimSpace(params[0])); err == nil && v >= 0 {
				days = v
			}
		}
		if len(params) >= 2 {
			searchBasesRaw = strings.TrimSpace(params[1])
		}
		if len(params) >= 3 {
			server = strings.TrimSpace(params[2])
		}
		searchBases := parseSearchBases(searchBasesRaw)

		users, err := getUsersAboutToBeDisabled(days, searchBases, server)
		out, _ := jsonStringOrEmptyArray(p, keyUsersAboutToBeDisabled, users, err)
		return out, nil


	default:
		return nil, plugin.UnsupportedMetricError
	}
}

func main() {
	// Simple standalone mode for testing:
	//   zabbix-agent2-ad-user-monitoring.exe --standalone <Key> [param1] [param2] ...
	// Example:
	//   ... --standalone PasswordExpiringUsers 14
	if len(os.Args) >= 2 {
		a := strings.TrimSpace(os.Args[1])
		if a == "--standalone" || a == "-standalone" || a == "standalone" {
			key := keyLockedOutUsers
			var params []string
			if len(os.Args) >= 3 {
				key = strings.TrimSpace(os.Args[2])
				if len(os.Args) > 3 {
					params = os.Args[3:]
				}
			}
			p := &impl{}
			v, _ := p.Export(key, params, nil)
			fmt.Println(v)
			os.Exit(0)
		}
	}

	if err := runPlugin(); err != nil {
		panic(err)
	}
}

func runPlugin() error {
	if err := plugin.RegisterMetrics(&pluginImpl, pluginName, keyLockedOutUsers, "Returns currently locked-out Active Directory users as JSON."); err != nil {
		return err
	}
	if err := plugin.RegisterMetrics(&pluginImpl, pluginName, keyDisabledUsers, "Returns disabled Active Directory users (disabled longer than N days) as JSON."); err != nil {
		return err
	}
	if err := plugin.RegisterMetrics(&pluginImpl, pluginName, keyPasswordExpiringUsers, "Returns Active Directory users whose passwords will expire within N days as JSON."); err != nil {
		return err
	}
	if err := plugin.RegisterMetrics(&pluginImpl, pluginName, keyUsersAboutToBeDisabled, "Returns Active Directory users whose accounts will expire (accountExpires) within N days as JSON."); err != nil {
		return err
	}

	h, err := container.NewHandler(pluginName)
	if err != nil {
		return err
	}
	pluginImpl.Logger = h
	return h.Execute()
}

// ---- WinLDAP minimal wrapper (SSPI/Negotiate) ----

const (
	ldapPort = 389

	ldapScopeBase    = 0
	ldapScopeSubtree = 2

	ldapOptProtocolVersion = 0x11

	ldapAuthOtherKind = 0x86
	ldapAuthNegotiate = ldapAuthOtherKind | 0x0400 // from winldap.h

	ldapFilterError = 87
)

type ldapError struct {
	op   string
	code uint32
}

func (e ldapError) Error() string {
	return fmt.Sprintf("%s failed (code=%d)", e.op, e.code)
}

type berval struct {
	bvLen uint32
	_     uint32 // padding on 64-bit
	bvVal *byte
}

var (
	wldap32               = windows.NewLazySystemDLL("wldap32.dll")
	procLdapInitW         = wldap32.NewProc("ldap_initW")
	procLdapSetOptionW    = wldap32.NewProc("ldap_set_optionW")
	procLdapBindSW        = wldap32.NewProc("ldap_bind_sW")
	procLdapSearchSW      = wldap32.NewProc("ldap_search_sW")
	procLdapFirstEntry    = wldap32.NewProc("ldap_first_entry")
	procLdapNextEntry     = wldap32.NewProc("ldap_next_entry")
	procLdapGetDnW        = wldap32.NewProc("ldap_get_dnW")
	procLdapMemFreeW      = wldap32.NewProc("ldap_memfreeW")
	procLdapGetValuesW    = wldap32.NewProc("ldap_get_valuesW")
	procLdapValueFreeW    = wldap32.NewProc("ldap_value_freeW")
	procLdapGetValuesLenW = wldap32.NewProc("ldap_get_values_lenW")
	procLdapValueFreeLen  = wldap32.NewProc("ldap_value_free_len")
	procLdapMsgFree       = wldap32.NewProc("ldap_msgfree")
	procLdapUnbind        = wldap32.NewProc("ldap_unbind")
)

type ldapConn struct {
	ld uintptr
}

func ldapDial(server string) (*ldapConn, error) {
	var hostPtr *uint16
	if strings.TrimSpace(server) != "" {
		p, err := windows.UTF16PtrFromString(server)
		if err != nil {
			return nil, err
		}
		hostPtr = p
	}

	r, _, _ := procLdapInitW.Call(
		uintptr(unsafe.Pointer(hostPtr)),
		uintptr(ldapPort),
	)
	if r == 0 {
		return nil, fmt.Errorf("ldap_initW failed")
	}
	ld := r

	// Set LDAP v3
	ver := uint32(3)
	rr, _, _ := procLdapSetOptionW.Call(
		ld,
		uintptr(ldapOptProtocolVersion),
		uintptr(unsafe.Pointer(&ver)),
	)
	if rr != 0 {
		_, _, _ = procLdapUnbind.Call(ld)
		return nil, ldapError{op: "ldap_set_optionW(LDAP_OPT_PROTOCOL_VERSION)", code: uint32(rr)}
	}

	// Bind using current security context (SSPI/Negotiate).
	rr, _, _ = procLdapBindSW.Call(
		ld,
		0,
		0,
		uintptr(ldapAuthNegotiate),
	)
	if rr != 0 {
		_, _, _ = procLdapUnbind.Call(ld)
		return nil, ldapError{op: "ldap_bind_sW", code: uint32(rr)}
	}

	return &ldapConn{ld: ld}, nil
}

func (c *ldapConn) close() {
	if c == nil || c.ld == 0 {
		return
	}
	_, _, _ = procLdapUnbind.Call(c.ld)
	c.ld = 0
}

func (c *ldapConn) search(baseDN string, scope uint32, filter string, attrs []string) (uintptr, error) {
	basePtr, _ := windows.UTF16PtrFromString(baseDN)
	filterPtr, _ := windows.UTF16PtrFromString(filter)

	attrPtrs := make([]*uint16, 0, len(attrs)+1)
	for _, a := range attrs {
		p, err := windows.UTF16PtrFromString(a)
		if err != nil {
			return 0, err
		}
		attrPtrs = append(attrPtrs, p)
	}
	attrPtrs = append(attrPtrs, nil)

	var res uintptr
	rr, _, _ := procLdapSearchSW.Call(
		c.ld,
		uintptr(unsafe.Pointer(basePtr)),
		uintptr(scope),
		uintptr(unsafe.Pointer(filterPtr)),
		uintptr(unsafe.Pointer(&attrPtrs[0])),
		0,
		uintptr(unsafe.Pointer(&res)),
	)
	if rr != 0 {
		return 0, ldapError{op: "ldap_search_sW", code: uint32(rr)}
	}
	return res, nil
}

func (c *ldapConn) getFirstStringValue(entry uintptr, attr string) string {
	values := c.getStringValues(entry, attr)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func (c *ldapConn) getStringValues(entry uintptr, attr string) []string {
	attrPtr, err := windows.UTF16PtrFromString(attr)
	if err != nil {
		return nil
	}

	r, _, _ := procLdapGetValuesW.Call(
		c.ld,
		entry,
		uintptr(unsafe.Pointer(attrPtr)),
	)
	if r == 0 {
		return nil
	}
	defer func() {
		_, _, _ = procLdapValueFreeW.Call(r)
	}()

	var out []string
	ptrSize := unsafe.Sizeof(uintptr(0))
	for i := 0; ; i++ {
		p := *(*uintptr)(unsafe.Pointer(r + uintptr(i)*ptrSize))
		if p == 0 {
			break
		}
		out = append(out, windows.UTF16PtrToString((*uint16)(unsafe.Pointer(p))))
	}
	return out
}

func (c *ldapConn) getBinaryValues(entry uintptr, attr string) [][]byte {
	attrPtr, err := windows.UTF16PtrFromString(attr)
	if err != nil {
		return nil
	}

	r, _, _ := procLdapGetValuesLenW.Call(
		c.ld,
		entry,
		uintptr(unsafe.Pointer(attrPtr)),
	)
	if r == 0 {
		return nil
	}
	defer func() {
		_, _, _ = procLdapValueFreeLen.Call(r)
	}()

	var out [][]byte
	ptrSize := unsafe.Sizeof(uintptr(0))
	for i := 0; ; i++ {
		p := *(*uintptr)(unsafe.Pointer(r + uintptr(i)*ptrSize))
		if p == 0 {
			break
		}
		bv := (*berval)(unsafe.Pointer(p))
		if bv.bvVal == nil || bv.bvLen == 0 {
			continue
		}
		b := unsafe.Slice(bv.bvVal, bv.bvLen)
		cp := make([]byte, len(b))
		copy(cp, b)
		out = append(out, cp)
	}
	return out
}

func (c *ldapConn) getDN(entry uintptr) string {
	r, _, _ := procLdapGetDnW.Call(c.ld, entry)
	if r == 0 {
		return ""
	}
	defer func() {
		_, _, _ = procLdapMemFreeW.Call(r)
	}()
	return windows.UTF16PtrToString((*uint16)(unsafe.Pointer(r)))
}

func (c *ldapConn) getDefaultNamingContext() (string, error) {
	// RootDSE: base DN is empty string
	res, err := c.search("", ldapScopeBase, "(objectClass=*)", []string{"defaultNamingContext"})
	if err != nil {
		return "", err
	}
	defer func() {
		_, _, _ = procLdapMsgFree.Call(res)
	}()

	entry, _, _ := procLdapFirstEntry.Call(c.ld, res)
	if entry == 0 {
		return "", fmt.Errorf("no RootDSE entry")
	}

	dnc := strings.TrimSpace(c.getFirstStringValue(entry, "defaultNamingContext"))
	if dnc == "" {
		return "", fmt.Errorf("defaultNamingContext is empty")
	}
	return dnc, nil
}

// ---- Shared helpers ----

const windowsToUnix100ns = 116444736000000000

func filetimeToTime(ft uint64) time.Time {
	// ft = 100ns ticks since 1601-01-01
	if ft <= windowsToUnix100ns {
		return time.Unix(0, 0).UTC()
	}
	unix100ns := int64(ft - windowsToUnix100ns)
	return time.Unix(0, unix100ns*100).UTC()
}

func timeToFiletime(t time.Time) uint64 {
	// 100ns ticks since 1601-01-01
	unix100ns := uint64(t.UTC().UnixNano() / 100)
	return unix100ns + windowsToUnix100ns
}

func parseGeneralizedTime(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty")
	}

	layouts := []string{
		"20060102150405.0Z",
		"20060102150405.00Z",
		"20060102150405.000Z",
		"20060102150405.0000Z",
		"20060102150405.00000Z",
		"20060102150405.000000Z",
		"20060102150405Z",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			tt := t.UTC()
			return &tt, nil
		}
	}
	return nil, fmt.Errorf("unsupported generalizedTime: %q", s)
}

func resolveSearchBases(conn *ldapConn, searchBases []string) ([]string, error) {
	// Remove empty entries first.
	clean := make([]string, 0, len(searchBases))
	for _, b := range searchBases {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		clean = append(clean, b)
	}
	if len(clean) > 0 {
		return clean, nil
	}

	// Default: entire domain naming context.
	dnc, err := conn.getDefaultNamingContext()
	if err != nil {
		return nil, err
	}
	return []string{dnc}, nil
}

// ---- Locked-out users logic ----

const (
	// UF_LOCKOUT bit in msDS-User-Account-Control-Computed
	ufLockout = 0x0010
	// ACCOUNTDISABLE bit in userAccountControl
	uacAccountDisable = 0x0002
)

func getLockedOutUsers(searchBases []string, server string) ([]lockedOutUser, error) {
	conn, err := ldapDial(server)
	if err != nil {
		return nil, err
	}
	defer conn.close()

	bases, err := resolveSearchBases(conn, searchBases)
	if err != nil {
		return nil, err
	}

	// Use lockoutTime>=1 to narrow results; then verify with msDS-User-Account-Control-Computed lockout bit.
	filter := "(&(objectCategory=person)(objectClass=user)(lockoutTime>=1))"

	attrs := []string{
		"sAMAccountName",
		"userPrincipalName",
		"distinguishedName",
		"userAccountControl",
		"msDS-User-Account-Control-Computed",
		"lockoutTime",
		"whenCreated",
		"whenChanged",
	}

	var out []lockedOutUser
	seen := make(map[string]struct{}, 128)

	for _, baseDN := range bases {
		res, err := conn.search(baseDN, ldapScopeSubtree, filter, attrs)
		if err != nil {
			return nil, err
		}

		for entry, _, _ := procLdapFirstEntry.Call(conn.ld, res); entry != 0; entry, _, _ = procLdapNextEntry.Call(conn.ld, res, entry) {
			sam := strings.TrimSpace(conn.getFirstStringValue(entry, "sAMAccountName"))
			upn := strings.TrimSpace(conn.getFirstStringValue(entry, "userPrincipalName"))
			dn := strings.TrimSpace(conn.getFirstStringValue(entry, "distinguishedName"))
			if dn == "" {
				dn = conn.getDN(entry)
			}

			uacStr := strings.TrimSpace(conn.getFirstStringValue(entry, "userAccountControl"))
			uac, _ := strconv.Atoi(uacStr)

			compStr := strings.TrimSpace(conn.getFirstStringValue(entry, "msDS-User-Account-Control-Computed"))
			comp, _ := strconv.Atoi(compStr)
			if (comp & ufLockout) == 0 {
				// Not currently locked out (lockout may have expired)
				continue
			}

			key := strings.ToLower(strings.TrimSpace(dn))
			if key == "" {
				key = strings.ToLower(strings.TrimSpace(sam))
			}
			if key != "" {
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
			}

			enabled := (uac & uacAccountDisable) == 0

			lockoutStr := strings.TrimSpace(conn.getFirstStringValue(entry, "lockoutTime"))
			var lockoutOut *string
			if lockoutStr != "" {
				if v, err := strconv.ParseUint(lockoutStr, 10, 64); err == nil && v > 0 {
					t := filetimeToTime(v)
					s := t.UTC().Format(time.RFC3339Nano)
					lockoutOut = &s
				}
			}

			whenCreatedStr := strings.TrimSpace(conn.getFirstStringValue(entry, "whenCreated"))
			whenChangedStr := strings.TrimSpace(conn.getFirstStringValue(entry, "whenChanged"))

			var whenCreatedOut *string
			if t, err := parseGeneralizedTime(whenCreatedStr); err == nil && t != nil {
				s := t.UTC().Format(time.RFC3339Nano)
				whenCreatedOut = &s
			}

			var whenChangedOut *string
			if t, err := parseGeneralizedTime(whenChangedStr); err == nil && t != nil {
				s := t.UTC().Format(time.RFC3339Nano)
				whenChangedOut = &s
			}

			out = append(out, lockedOutUser{
				SamAccountName:     sam,
				UserPrincipalName:  upn,
				DistinguishedName:  dn,
				Enabled:            enabled,
				LockedOut:          true,
				LockoutTimeUtc:     lockoutOut,
				WhenCreatedUtc:     whenCreatedOut,
				WhenChangedUtc:     whenChangedOut,
				UserAccountControl: uac,
			})
		}

		_, _, _ = procLdapMsgFree.Call(res)
	}

	return out, nil
}

// ---- Disabled users logic ----

func getDisabledUsers(days int, searchBases []string, server string) ([]disabledUser, error) {
	conn, err := ldapDial(server)
	if err != nil {
		return nil, err
	}
	defer conn.close()

	bases, err := resolveSearchBases(conn, searchBases)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	cutoff := now.AddDate(0, 0, -days)

	// userAccountControl bit 2 (ACCOUNTDISABLE) set
	filter := "(&(objectCategory=person)(objectClass=user)(userAccountControl:1.2.840.113556.1.4.803:=2))"

	attrs := []string{
		"sAMAccountName",
		"userPrincipalName",
		"distinguishedName",
		"userAccountControl",
		"whenCreated",
		"whenChanged",
		"msDS-ReplAttributeMetaData;binary",
	}

	var out []disabledUser
	seen := make(map[string]struct{}, 256)

	for _, baseDN := range bases {
		res, err := conn.search(baseDN, ldapScopeSubtree, filter, attrs)
		if err != nil {
			return nil, err
		}

		for entry, _, _ := procLdapFirstEntry.Call(conn.ld, res); entry != 0; entry, _, _ = procLdapNextEntry.Call(conn.ld, res, entry) {
			sam := strings.TrimSpace(conn.getFirstStringValue(entry, "sAMAccountName"))
			upn := strings.TrimSpace(conn.getFirstStringValue(entry, "userPrincipalName"))
			dn := strings.TrimSpace(conn.getFirstStringValue(entry, "distinguishedName"))
			if dn == "" {
				dn = conn.getDN(entry)
			}

			key := strings.ToLower(strings.TrimSpace(dn))
			if key == "" {
				key = strings.ToLower(strings.TrimSpace(sam))
			}
			if key != "" {
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
			}

			uacStr := strings.TrimSpace(conn.getFirstStringValue(entry, "userAccountControl"))
			uac, _ := strconv.Atoi(uacStr)

			whenCreatedStr := strings.TrimSpace(conn.getFirstStringValue(entry, "whenCreated"))
			whenChangedStr := strings.TrimSpace(conn.getFirstStringValue(entry, "whenChanged"))

			var whenCreatedOut *string
			if t, err := parseGeneralizedTime(whenCreatedStr); err == nil && t != nil {
				s := t.UTC().Format(time.RFC3339Nano)
				whenCreatedOut = &s
			}

			var whenChangedTime *time.Time
			var whenChangedOut *string
			if t, err := parseGeneralizedTime(whenChangedStr); err == nil && t != nil {
				tt := t.UTC()
				whenChangedTime = &tt
				s := tt.Format(time.RFC3339Nano)
				whenChangedOut = &s
			}

			// Find last originating change time for userAccountControl in msDS-ReplAttributeMetaData;binary
			metaBlobs := conn.getBinaryValues(entry, "msDS-ReplAttributeMetaData;binary")
			var disabledSince *time.Time
			for _, b := range metaBlobs {
				attr, ft, ok := parseReplAttrMetaDataBlob(b)
				if !ok {
					continue
				}
				if strings.EqualFold(attr, "userAccountControl") {
					t := filetimeToTime(ft)
					disabledSince = &t
					break
				}
			}

			// If we couldn't read metadata (permissions vary), fall back to whenChanged (best effort).
			if disabledSince == nil && whenChangedTime != nil {
				disabledSince = whenChangedTime
			}

			// If we still can't determine disabled-since, skip.
			if disabledSince == nil {
				continue
			}

			// Filter by cutoff (disabled longer than N days)
			if disabledSince.After(cutoff) {
				continue
			}

			disabledSinceStr := disabledSince.UTC().Format(time.RFC3339Nano)
			daysDisabled := int(now.Sub(*disabledSince).Hours() / 24)

			out = append(out, disabledUser{
				SamAccountName:     sam,
				UserPrincipalName:  upn,
				DistinguishedName:  dn,
				Enabled:            false,
				DisabledSinceUtc:   &disabledSinceStr,
				DaysDisabled:       &daysDisabled,
				WhenCreatedUtc:     whenCreatedOut,
				WhenChangedUtc:     whenChangedOut,
				UserAccountControl: uac,
			})
		}

		_, _, _ = procLdapMsgFree.Call(res)
	}

	return out, nil
}

// ---- Password expiring users logic ----

const (
	uacDontExpirePassword = 0x10000
	adNeverExpiresInt8    = 9223372036854775807 // 0x7FFF... max int64, used by AD for "never" on some Integer8 values
)

func getPasswordExpiringUsers(days int, searchBases []string, server string) ([]passwordExpiringUser, error) {
	conn, err := ldapDial(server)
	if err != nil {
		return nil, err
	}
	defer conn.close()

	bases, err := resolveSearchBases(conn, searchBases)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	threshold := now.AddDate(0, 0, days)

	nowFt := timeToFiletime(now)
	thrFt := timeToFiletime(threshold)

	// Preferred: filter server-side on computed expiry.
	filterWithComputed := fmt.Sprintf(
		"(&(objectCategory=person)(objectClass=user)"+
			"(!(userAccountControl:1.2.840.113556.1.4.803:=2))"+
			"(!(userAccountControl:1.2.840.113556.1.4.803:=%d))"+
			"(pwdLastSet>=1)"+
			"(msDS-UserPasswordExpiryTimeComputed>=%d)"+
			"(msDS-UserPasswordExpiryTimeComputed<=%d)"+
			")",
		uacDontExpirePassword,
		nowFt,
		thrFt,
	)

	// Fallback: do not use msDS-UserPasswordExpiryTimeComputed in the LDAP filter
	filterFallback := fmt.Sprintf(
		"(&(objectCategory=person)(objectClass=user)"+
			"(!(userAccountControl:1.2.840.113556.1.4.803:=2))"+
			"(!(userAccountControl:1.2.840.113556.1.4.803:=%d))"+
			"(pwdLastSet>=1)"+
			")",
		uacDontExpirePassword,
	)

	attrs := []string{
		"sAMAccountName",
		"userPrincipalName",
		"distinguishedName",
		"userAccountControl",
		"msDS-UserPasswordExpiryTimeComputed",
		"pwdLastSet",
	}

	var out []passwordExpiringUser
	seen := make(map[string]struct{}, 256)

	for _, baseDN := range bases {
		res, err := conn.search(baseDN, ldapScopeSubtree, filterWithComputed, attrs)
		if err != nil {
			// If the directory doesn't accept the filter (common for some constructed attributes), retry.
			if le, ok := err.(ldapError); ok && le.code == ldapFilterError {
				res, err = conn.search(baseDN, ldapScopeSubtree, filterFallback, attrs)
			}
		}
		if err != nil {
			return nil, err
		}

		for entry, _, _ := procLdapFirstEntry.Call(conn.ld, res); entry != 0; entry, _, _ = procLdapNextEntry.Call(conn.ld, res, entry) {
			sam := strings.TrimSpace(conn.getFirstStringValue(entry, "sAMAccountName"))
			if sam == "" {
				continue
			}

			upn := strings.TrimSpace(conn.getFirstStringValue(entry, "userPrincipalName"))
			dn := strings.TrimSpace(conn.getFirstStringValue(entry, "distinguishedName"))
			if dn == "" {
				dn = conn.getDN(entry)
			}

			key := strings.ToLower(strings.TrimSpace(dn))
			if key == "" {
				key = strings.ToLower(strings.TrimSpace(sam))
			}
			if key != "" {
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
			}

			uacStr := strings.TrimSpace(conn.getFirstStringValue(entry, "userAccountControl"))
			uac, _ := strconv.Atoi(uacStr)
			enabled := (uac & uacAccountDisable) == 0

			expStr := strings.TrimSpace(conn.getFirstStringValue(entry, "msDS-UserPasswordExpiryTimeComputed"))
			if expStr == "" {
				continue
			}

			ft, err := strconv.ParseUint(expStr, 10, 64)
			if err != nil {
				continue
			}
			if ft == 0 || ft >= adNeverExpiresInt8 {
				// Password never expires / not applicable
				continue
			}

			expiry := filetimeToTime(ft)
			// Only include "about to expire" in the future window.
			if expiry.Before(now) {
				continue
			}
			if expiry.After(threshold) {
				continue
			}

			expiryStr := expiry.UTC().Format(time.RFC3339Nano)
			daysToExpire := int(expiry.Sub(now).Hours() / 24)

			out = append(out, passwordExpiringUser{
				SamAccountName:     sam,
				UserPrincipalName:  upn,
				DistinguishedName:  dn,
				Enabled:            enabled,
				PasswordExpiresUtc: &expiryStr,
				DaysToExpire:       &daysToExpire,
				UserAccountControl: uac,
			})
		}

		_, _, _ = procLdapMsgFree.Call(res)
	}

	return out, nil
}

// ---- Users about to be disabled (accountExpires) ----

func getUsersAboutToBeDisabled(days int, searchBases []string, server string) ([]aboutToBeDisabledUser, error) {
	conn, err := ldapDial(server)
	if err != nil {
		return nil, err
	}
	defer conn.close()

	bases, err := resolveSearchBases(conn, searchBases)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	threshold := now.AddDate(0, 0, days)

	nowFt := timeToFiletime(now)
	thrFt := timeToFiletime(threshold)

	filter := fmt.Sprintf(
		"(&(objectCategory=person)(objectClass=user)"+
			"(!(userAccountControl:1.2.840.113556.1.4.803:=2))"+
			"(accountExpires>=%d)"+
			"(accountExpires<=%d)"+
			")",
		nowFt,
		thrFt,
	)

	attrs := []string{
		"sAMAccountName",
		"userPrincipalName",
		"distinguishedName",
		"userAccountControl",
		"accountExpires",
	}

	var out []aboutToBeDisabledUser
	seen := make(map[string]struct{}, 256)

	for _, baseDN := range bases {
		res, err := conn.search(baseDN, ldapScopeSubtree, filter, attrs)
		if err != nil {
			return nil, err
		}

		for entry, _, _ := procLdapFirstEntry.Call(conn.ld, res); entry != 0; entry, _, _ = procLdapNextEntry.Call(conn.ld, res, entry) {
			sam := strings.TrimSpace(conn.getFirstStringValue(entry, "sAMAccountName"))
			if sam == "" {
				continue
			}

			upn := strings.TrimSpace(conn.getFirstStringValue(entry, "userPrincipalName"))
			dn := strings.TrimSpace(conn.getFirstStringValue(entry, "distinguishedName"))
			if dn == "" {
				dn = conn.getDN(entry)
			}

			key := strings.ToLower(strings.TrimSpace(dn))
			if key == "" {
				key = strings.ToLower(strings.TrimSpace(sam))
			}
			if key != "" {
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
			}

			uacStr := strings.TrimSpace(conn.getFirstStringValue(entry, "userAccountControl"))
			uac, _ := strconv.Atoi(uacStr)
			enabled := (uac & uacAccountDisable) == 0

			expStr := strings.TrimSpace(conn.getFirstStringValue(entry, "accountExpires"))
			if expStr == "" {
				continue
			}

			ft, err := strconv.ParseUint(expStr, 10, 64)
			if err != nil {
				continue
			}
			if ft == 0 || ft >= adNeverExpiresInt8 {
				// Account never expires
				continue
			}

			expiry := filetimeToTime(ft)
			if expiry.Before(now) {
				continue
			}
			if expiry.After(threshold) {
				continue
			}

			expiryStr := expiry.UTC().Format(time.RFC3339Nano)
			daysToDisable := int(expiry.Sub(now).Hours() / 24)

			out = append(out, aboutToBeDisabledUser{
				SamAccountName:     sam,
				UserPrincipalName:  upn,
				DistinguishedName:  dn,
				Enabled:            enabled,
				AccountExpiresUtc:  &expiryStr,
				DaysToDisable:      &daysToDisable,
				UserAccountControl: uac,
			})
		}

		_, _, _ = procLdapMsgFree.Call(res)
	}

	return out, nil
}

// ---- DS_REPL_ATTR_META_DATA_BLOB parsing (msDS-ReplAttributeMetaData;binary) ----
//
// Layout (little-endian):
// DWORD oszAttributeName
// DWORD dwVersion
// FILETIME ftimeLastOriginatingChange (QWORD)
// UUID uuidLastOriginatingDsaInvocationID (16 bytes)
// USN usnOriginatingChange (QWORD)
// USN usnLocalChange (QWORD)
// DWORD oszLastOriginatingDsaDN
//
// Strings are UTF-16LE, offsets are byte offsets from the beginning of the struct.

func parseReplAttrMetaDataBlob(b []byte) (attrName string, lastChangeFiletime uint64, ok bool) {
	if len(b) < 52 {
		return "", 0, false
	}

	offName := binary.LittleEndian.Uint32(b[0:4])
	// FILETIME starts at offset 8
	ftLow := binary.LittleEndian.Uint32(b[8:12])
	ftHigh := binary.LittleEndian.Uint32(b[12:16])
	ft := (uint64(ftHigh) << 32) | uint64(ftLow)

	name := utf16leStringAt(b, offName)
	if name == "" {
		return "", 0, false
	}
	return name, ft, true
}

func utf16leStringAt(b []byte, off uint32) string {
	start := int(off)
	if start < 0 || start >= len(b) {
		return ""
	}
	// Read uint16 values until null
	u16s := make([]uint16, 0, 64)
	for i := start; i+1 < len(b); i += 2 {
		v := binary.LittleEndian.Uint16(b[i : i+2])
		if v == 0 {
			break
		}
		u16s = append(u16s, v)
	}
	if len(u16s) == 0 {
		return ""
	}
	return string(utf16.Decode(u16s))
}
