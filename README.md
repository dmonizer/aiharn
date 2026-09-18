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

The TUI also provides skill commands:

- `/skill` (no arguments) lists the installed skills for you only. The output
  is local transcript lines and is not sent to any agent.
- `/skill install <url>` reads `<aiharn_home>/prompts/skill-install.md`,
  expands `${URL}` and `${SKILLS_DIR}`, and sends the resulting prompt to the
  current/focused agent.

The repo provides `prompts/skill-install.md` as a template; copy it into
`<aiharn_home>/prompts/` if you want to use `/skill install`.
