package op

// Mail: run.sh's SMTP menu (8) — postfix relaying through Resend, and where
// the box's own notifications go.

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/box"
	"github.com/valolink/hestiascripts/internal/conf"
)

const (
	postfixCF  = "/etc/postfix/main.cf"
	saslPasswd = "/etc/postfix/sasl_passwd"
	genericMap = "/etc/postfix/generic"

	senderCanonical = "/etc/postfix/hs_sender_canonical"
	smtpHeaderCheck = "/etc/postfix/hs_smtp_header_checks"
	smtpReplyFilter = "/etc/postfix/hs_smtp_reply_filter"
)

func fqdn(ctx context.Context, env *check.Env) string {
	out, _ := env.Sys.Run(ctx, "hostname", "-f")
	if h := strings.TrimSpace(out); h != "" {
		return h
	}
	h, _ := os.Hostname()
	return h
}

func init() {
	register(Op{
		ID: "smtp-install", Title: "Install postfix for relaying", Section: "mail", Risk: Change,
		Note:    "postfix, the SASL modules for relay login, and mailutils (`mail`). Refused on a box where exim4 — Hestia's own mail server — is installed: apt would remove it to make room.",
		How:     "debconf is pre-answered (Internet Site, this host's FQDN as mailname) so apt installs without a dialog; if postfix is installed but has no main.cf, `dpkg-reconfigure` writes one with the same answers.",
		Undo:    "apt purge postfix libsasl2-modules mailutils.",
		Recheck: []string{"mail.delivery"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			if box.PkgInstalled(ctx, env.Sys, "exim4-daemon-heavy", "exim4-daemon-light") {
				return nil, fmt.Errorf("exim4 is installed (Hestia's mail server); installing postfix would remove it — configure a relay in exim instead (Hestia: v-add-sys-smtp-relay)")
			}
			var missing []string
			for _, p := range []string{"postfix", "libsasl2-modules", "mailutils"} {
				if !box.PkgInstalled(ctx, env.Sys, p) {
					missing = append(missing, p)
				}
			}
			h := fqdn(ctx, env)
			seed := Step{Why: "answer postfix's install questions (mailname " + h + ", Internet Site)",
				Argv: []string{"sh", "-c", `printf 'postfix postfix/mailname string %s\npostfix postfix/main_mailer_type string Internet Site\n' "$1" | debconf-set-selections`, "sh", h}}
			var steps []Step
			if len(missing) > 0 {
				steps = append(steps, seed, aptInstall("install", missing...))
			} else if !exists(env, postfixCF) {
				steps = append(steps, seed, Step{Why: "postfix has no main.cf", Argv: []string{"env", "DEBIAN_FRONTEND=noninteractive", "dpkg-reconfigure", "postfix"}})
			}
			return steps, nil
		},
	})
	register(Op{
		ID: "smtp-relay", Title: "Relay mail through Resend", Section: "mail", Risk: Change,
		Resolves: "mail.delivery", Applies: summaryHas("no SMTP relay"),
		Note: "Postfix sends everything through smtp.resend.com:587 with your API key. The key is typed masked and never appears in the plan, the command line, the action log or a backup — only in /etc/postfix/sasl_passwd (root-only), where postfix needs it.",
		How:  "The API key reaches hs in an environment variable and is written to /etc/postfix/sasl_passwd (umask 077) and hashed with postmap. main.cf is copied to /var/lib/hs/backups first, then `postconf -e` sets the relay, SASL login and mandatory TLS. A sender address rewrites root@, www-data@ and @<hostname> through /etc/postfix/generic, so cron and alert mail comes from a domain verified in Resend, and the Resend guard (see smtp-resend-guard) rewrites every other sender and paces deliveries. postfix reloads.",
		Undo: "Copy the kept main.cf back (path in the plan) and `systemctl reload postfix`; rm /etc/postfix/sasl_passwd*.",
		Fields: []Field{
			{Key: "api-key", Label: "Resend API key", Kind: Secret, Help: "re_… from resend.com/api-keys (sending access is enough).",
				Current: func(_ context.Context, env *check.Env, _ Target) string {
					if exists(env, saslPasswd) {
						return "a key is set"
					}
					return "none"
				},
				Validate: func(v string) error {
					if !strings.HasPrefix(v, "re_") {
						return fmt.Errorf("a Resend API key starts with re_")
					}
					return nil
				}},
			{Key: "sender", Label: "Send system mail as", Kind: Text, Optional: true, Pattern: emailRe,
				Help: "An address on a domain verified in Resend (noreply@…). Empty = no rewriting; system mail may bounce.",
				Current: func(_ context.Context, env *check.Env, _ Target) string {
					for _, l := range strings.Split(readFile(env, genericMap), "\n") {
						if f := strings.Fields(l); len(f) == 2 && f[0] == "root" {
							return f[1]
						}
					}
					return ""
				}},
		},
		Recheck: []string{"mail.delivery"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			if !have(env, "postconf") || !exists(env, postfixCF) {
				return nil, fmt.Errorf("postfix is not installed — Install postfix for relaying first")
			}
			if !box.PkgInstalled(ctx, env.Sys, "libsasl2-modules") {
				return nil, fmt.Errorf("libsasl2-modules is missing — Install postfix for relaying first")
			}
			bak := conf.BackupPath(postfixCF, time.Now())
			secret := "$" + SecretEnv("api-key")
			steps := []Step{
				{Why: "keep main.cf", Argv: []string{"install", "-D", "-m", "600", postfixCF, bak}},
				{Why: "the login (key from the environment, never on screen)", Argv: []string{"sh", "-c",
					`umask 077 && printf '[smtp.resend.com]:587 resend:%s\n' "` + secret + `" > ` + saslPasswd + ` && postmap ` + saslPasswd + ` && chmod 600 ` + saslPasswd + ` ` + saslPasswd + `.db && ls -l ` + saslPasswd + `*`}},
				{Why: "relay, SASL, mandatory TLS", Argv: []string{"postconf", "-e",
					"relayhost = [smtp.resend.com]:587", "smtp_sasl_auth_enable = yes", "smtp_sasl_password_maps = hash:" + saslPasswd,
					"smtp_sasl_security_options = noanonymous", "smtp_tls_security_level = encrypt", "smtp_tls_CAfile = /etc/ssl/certs/ca-certificates.crt"}},
			}
			if s := v["sender"]; s != "" {
				steps = append(steps,
					Step{Why: "system mail from " + s, Argv: []string{"sh", "-c",
						`printf 'root              %s\nwww-data          %s\n@%s          %s\n@localhost        %s\n' "$1" "$1" "$2" "$1" "$1" > ` + genericMap + ` && postmap ` + genericMap, "sh", s, fqdn(ctx, env)}},
					Step{Argv: []string{"postconf", "-e", "smtp_generic_maps = hash:" + genericMap}})
				steps = append(steps, resendGuardSteps(s)...)
			}
			return append(steps,
				Step{Why: "apply", Argv: []string{"systemctl", "reload-or-restart", "postfix"}},
				Step{Why: "now: send a test (Mail → Send a test email)", Argv: []string{"postconf", "relayhost", "smtp_generic_maps"}},
			), nil
		},
	})
	register(Op{
		ID: "smtp-resend-guard", Title: "Resend: send every sender as the box address, paced", Section: "mail", Risk: Change,
		Resolves: "mail.resend-guard",
		Note:     "Fleet 2026-09-30: Resend refused WordPress mail sent as admin@<site> (Hestia's PHP pools pass -f admin@<domain>) because only the sender address's own domain is verified — \"This API key is not authorized to send emails from <domain>\" — and answered bursts with a permanent \"550 Too many requests\", so postfix dropped them; the bounce notices then failed as \"Invalid from\".",
		How:      "Three regexp tables in /etc/postfix: hs_sender_canonical (envelope senders not on the verified domain become the box address; sender_canonical_classes = envelope_sender only), hs_smtp_header_checks (only the From: header, display name kept — Reply-To is never touched, so replies still reach the original person) and hs_smtp_reply_filter (Resend's 550 Too many requests becomes a 450, so postfix retries). smtp_destination_rate_delay = 1s keeps one delivery per second. main.cf is backed up first; postfix reloads.",
		Undo:     "Copy the kept main.cf back (path in the plan) and `systemctl reload postfix`; the three hs_* files can stay or be removed.",
		Fields: []Field{{Key: "sender", Label: "Box address (on the verified domain)", Kind: Text, Pattern: emailRe,
			Help: "The address system mail already uses — the root line of /etc/postfix/generic.",
			Current: func(_ context.Context, env *check.Env, _ Target) string {
				for _, l := range strings.Split(readFile(env, genericMap), "\n") {
					if f := strings.Fields(l); len(f) == 2 && f[0] == "root" {
						return f[1]
					}
				}
				return ""
			}}},
		Recheck: []string{"mail.resend-guard", "mail.delivery"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			if !strings.Contains(box.Relay(ctx, env), "resend.com") {
				return nil, fmt.Errorf("postfix does not relay through Resend — Relay mail through Resend first")
			}
			s := v["sender"]
			if !strings.Contains(s, "@") {
				return nil, fmt.Errorf("the box address needs a domain (%q)", s)
			}
			steps := []Step{{Why: "keep main.cf", Argv: []string{"install", "-D", "-m", "600", postfixCF, conf.BackupPath(postfixCF, time.Now())}}}
			steps = append(steps, resendGuardSteps(s)...)
			return append(steps,
				Step{Why: "check and apply", Argv: []string{"sh", "-c", "postfix check && systemctl reload-or-restart postfix"}},
				Step{Why: "an unverified sender now maps to", Argv: []string{"postmap", "-q", "admin@example.fi", "regexp:" + senderCanonical}},
			), nil
		},
	})
	register(Op{
		ID: "smtp-recipients", Title: "Where the box's notifications go", Section: "mail", Risk: Change,
		Note: "One address for root/cron mail, Maldet alerts, Fail2ban, unattended-upgrades reports and the Hestia admin contact. Fail2ban keeps its ban-only action — run.sh switched on a mail per ban here, which flooded the inbox and its own repair then undid.",
		How:  "root's line in /etc/aliases (then newaliases); `hs conf set` for Maldet's email_addr and Fail2ban's destemail ([DEFAULT] in jail.local) then `hs f2b reload`; the Unattended-Upgrade::Mail line (and MailReport on-change) in 50unattended-upgrades; `v-change-user-contact admin`. Only what is installed is touched; each file's change is printed.",
		Undo: "Run it again with the previous address.",
		Fields: []Field{{Key: "email", Label: "Address", Kind: Text, Pattern: emailRe,
			Current: func(_ context.Context, env *check.Env, _ Target) string {
				for _, l := range strings.Split(readFile(env, "/etc/aliases"), "\n") {
					if strings.HasPrefix(l, "root:") {
						return "root → " + strings.TrimSpace(strings.TrimPrefix(l, "root:"))
					}
				}
				return "root alias not set"
			}}},
		Recheck: []string{"mail.delivery"},
		Plan: func(_ context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			e := v["email"]
			steps := []Step{{Why: "root and cron mail", Argv: []string{"sh", "-c",
				`if grep -q '^root:' /etc/aliases; then sed -i "s|^root:.*|root: $1|" /etc/aliases; else echo "root: $1" >> /etc/aliases; fi; newaliases; grep '^root:' /etc/aliases`, "sh", e}}}
			if exists(env, maldetConf) {
				steps = append(steps, confSet("Maldet alerts", maldetConf, conf.Opts{Style: conf.Shell}, "email_alert", "1", "email_addr", e))
			}
			if exists(env, "/etc/fail2ban/jail.local") {
				steps = append(steps, confSet("Fail2ban (ban-only action unchanged)", "/etc/fail2ban/jail.local", conf.Opts{Section: "DEFAULT", Style: conf.INI}, "destemail", e),
					Step{Argv: []string{self(), "f2b", "reload"}})
			}
			if exists(env, uuFile) {
				steps = append(steps, Step{Why: "unattended-upgrades reports", Argv: []string{"sh", "-c",
					`f=` + uuFile + `; if grep -q 'Unattended-Upgrade::Mail ' "$f"; then sed -i "s|.*Unattended-Upgrade::Mail \".*\";|Unattended-Upgrade::Mail \"$1\";|" "$f"; else echo "Unattended-Upgrade::Mail \"$1\";" >> "$f"; fi; grep -q '^Unattended-Upgrade::MailReport' "$f" || echo 'Unattended-Upgrade::MailReport "on-change";' >> "$f"; grep -n 'Unattended-Upgrade::Mail' "$f"`, "sh", e}})
			}
			if exists(env, bin+"v-change-user-contact") {
				steps = append(steps, Step{Why: "Hestia admin contact", Argv: []string{bin + "v-change-user-contact", "admin", e}})
			}
			return steps, nil
		},
	})
	register(Op{
		ID: "smtp-test", Title: "Send a test email", Section: "mail", Risk: Change,
		Resolves: "mail.delivery", Applies: func(r check.Result) bool {
			return strings.Contains(r.Summary, "failed") || strings.Contains(r.Summary, "no delivery seen")
		},
		Note:    "Sends one message and shows postfix's log for it — status=sent means the relay accepted it.",
		How:     "`mail -s` hands the message to postfix; after a few seconds the journal lines postfix wrote since are shown.",
		Undo:    "Nothing to undo (one email is sent).",
		Fields:  []Field{{Key: "to", Label: "Send to", Kind: Text, Pattern: emailRe}},
		Recheck: []string{"mail.delivery", "mail.queue"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			h := fqdn(ctx, env)
			sender := "mail"
			if !have(env, "mail") {
				return nil, fmt.Errorf("the mail command is missing — Install postfix for relaying (mailutils)")
			}
			return []Step{
				{Why: "hand it to postfix", Argv: []string{"sh", "-c", `echo "Test email from $2 via its SMTP relay (hs)." | ` + sender + ` -s "hs SMTP test — $2 — $(date '+%Y-%m-%d %H:%M')" "$1"`, "sh", v["to"], h}},
				{Why: "what postfix did with it", Argv: []string{"sh", "-c", "sleep 5; journalctl --since=-1min --no-pager -o short -t postfix/smtp -t postfix/qmgr -t postfix/cleanup -t postfix/pickup | tail -12"}},
			}, nil
		},
	})
}

// ResendGuardFiles is the content of the three postfix tables for a box
// address on Resend's verified domain (the part after @).
func ResendGuardFiles(sender string) (canonical, headers, reply string) {
	dom := regexp.QuoteMeta(sender[strings.LastIndex(sender, "@")+1:])
	canonical = "# hs: Resend accepts only its verified sender domain (" + dom + ").\n" +
		"# Envelope senders on other domains (admin@<site> from Hestia's PHP pools, root@<host>) send as the box address.\n" +
		"/^(.*@" + dom + ")$/\t${1}\n" +
		"/^.+$/\t" + sender + "\n"
	headers = "# hs: only the From header, display name kept; Reply-To is not touched.\n" +
		"/^From:.*@" + dom + ">?[[:space:]]*$/\tDUNNO\n" +
		"/^From:[[:space:]]*\"?([^\"<]*[^\"< ])\"?[[:space:]]*<[^>]*>[[:space:]]*$/\tREPLACE From: \"${1}\" <" + sender + ">\n" +
		"/^From:[[:space:]]*[^[:space:]]+[[:space:]]*\\((.*)\\)[[:space:]]*$/\tREPLACE From: \"${1}\" <" + sender + ">\n" +
		"/^From:.*$/\tREPLACE From: <" + sender + ">\n"
	reply = "# hs: Resend answers rate limiting with a permanent 550; make it a retry.\n" +
		"/^550([ -].*Too many requests.*)$/\t450${1}\n"
	return
}

func resendGuardSteps(sender string) []Step {
	c, h, r := ResendGuardFiles(sender)
	write := func(path, content string) Step {
		return Step{Why: "write " + path + ":\n" + strings.TrimRight(content, "\n"),
			Argv: []string{"sh", "-c", `printf '%s' "$1" > "$2" && chmod 644 "$2"`, "sh", content, path}}
	}
	return []Step{
		write(senderCanonical, c), write(smtpHeaderCheck, h), write(smtpReplyFilter, r),
		{Why: "use them; one delivery per second", Argv: []string{"postconf", "-e",
			"sender_canonical_maps = regexp:" + senderCanonical, "sender_canonical_classes = envelope_sender",
			"smtp_header_checks = regexp:" + smtpHeaderCheck, "smtp_reply_filter = regexp:" + smtpReplyFilter,
			"smtp_destination_rate_delay = 1s"}},
	}
}
