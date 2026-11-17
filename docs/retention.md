[//]: # (Generated from scripts/retentiondocs/retention.md.tmpl by task docs:gen. Edit the template.)

# Retention policies

`ark forget` keeps the snapshots a policy selects and removes the rest. The examples on this page are computed by ark itself: each shows a history of snapshots, what a policy keeps of it and the reasons `ark forget --json` gives. They count days, weeks, months and years in UTC; ark counts them in the local time zone of the machine that runs forget.

## Keep daily

`--keep-daily 3` keeps the newest snapshot of each of the three most recent days that have a snapshot.

| Snapshot | Result | Reason |
|---|---|---|
| Sun 22 Mar 18:00 | kept | `daily snapshot` |
| Fri 20 Mar 18:00 | kept | `daily snapshot` |
| Thu 19 Mar 18:00 | kept | `daily snapshot` |
| Wed 18 Mar 18:00 | removed | |
| Wed 18 Mar 13:00 | removed | |
| Wed 18 Mar 08:00 | removed | |
| Mon 16 Mar 18:00 | removed | |
| Fri 13 Mar 18:00 | removed | |
| Thu 12 Mar 18:00 | removed | |
| Thu 12 Mar 09:00 | removed | |
| Tue 10 Mar 18:00 | removed | |
| Mon 09 Mar 18:00 | removed | |
| Sat 07 Mar 18:00 | removed | |
| Fri 06 Mar 23:00 | removed | |
| Fri 06 Mar 18:00 | removed | |
| Fri 06 Mar 12:00 | removed | |
| Wed 04 Mar 18:00 | removed | |
| Tue 03 Mar 18:00 | removed | |
| Tue 03 Mar 09:00 | removed | |
| Mon 02 Mar 18:00 | removed | |

No snapshot falls on Sat 21 Mar; a day without one uses no count. The policy keeps Sun 22 Mar 18:00, Fri 20 Mar 18:00, Thu 19 Mar 18:00 and removes 17 other snapshots.
