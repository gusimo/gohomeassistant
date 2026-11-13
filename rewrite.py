export KEEP_LAST_SHA="$(git rev-parse HEAD)"

git filter-repo --commit-callback '
import os
from datetime import datetime, timezone

# Neuesten Commit ausnehmen
if commit.original_id == os.environ["KEEP_LAST_SHA"].encode():
    return

for field in ("author_date", "committer_date"):
    raw = getattr(commit, field)
    timestamp, tz = raw.split()

    # Lokales Commit-Datum einschließlich Zeitzone bestimmen
    sign = 1 if tz.startswith(b"+") else -1
    offset = sign * (int(tz[1:3]) * 3600 + int(tz[3:5]) * 60)

    local_date = datetime.fromtimestamp(
        int(timestamp) + offset, timezone.utc
    ).date()

    if local_date.isoformat() == "2026-11-10":
        # Exakt 48 Stunden zurück, Zeitzone bleibt erhalten
        new_timestamp = int(timestamp) - 172800
        setattr(
            commit,
            field,
            str(new_timestamp).encode() + b" " + tz
        )
'
