#!/bin/sh

# One backup, then wait. `|| true` so a failed run — an unreachable server, a rejected
# token — is logged and retried at the next interval rather than exiting the container.
while true; do
	/backup || true
	sleep "${BACKUP_INTERVAL:-3600}"
done
