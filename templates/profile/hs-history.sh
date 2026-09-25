# hs: shell history settings (templates/profile/hs-history.sh, hs op shell-history)
# A command typed with a leading space is not saved (keep secrets out of
# history); duplicates are not saved; every entry carries a timestamp so an
# incident can be reconstructed from it.
HISTCONTROL=ignoreboth
HISTTIMEFORMAT='%F %T '
HISTSIZE=10000
HISTFILESIZE=20000
