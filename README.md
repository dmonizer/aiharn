# aiharn

## Skills

System prompts can include `${SKILLS_INDEX}`, which is replaced at startup with
the contents of `<aiharn_home>/skills/index.md` (capped at 4 MiB).

```
<aiharn_home>/skills/index.md           # list of skills + descriptions
<aiharn_home>/skills/<skill-name>/SKILL.md
```

`aiharn_home` defaults to your home directory. The `skills/` directory is
optional unless a prompt references `${SKILLS_INDEX}`.
