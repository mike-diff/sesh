#!/bin/sh
# $1 is the task directory. Pass: 5 attempts; sleeps 50+100+200+300 = 650 ms.
grep -Eq '^attempts:[[:space:]]*5[[:space:]]*$' ANSWER.md || { echo "attempts wrong or missing"; exit 1; }
grep -Eq '^sleep_ms:[[:space:]]*650[[:space:]]*$' ANSWER.md || { echo "sleep_ms wrong or missing"; exit 1; }
