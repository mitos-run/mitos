# mitos SDK demo recording (Charm VHS).  Render with:  make demo
#
# Records a real Python session against a mitos sandbox. By default it uses the
# installed `mitos-run` SDK and whatever MITOS_BASE_URL / MITOS_API_KEY point at.
# The launch GIF was rendered offline against a local SDK-compatible stub by
# setting MITOS_DEMO_PYTHONPATH (no cluster or key needed); the commands and
# outputs shown are the real SDK's behavior for those calls.
Output docs/assets/demo.gif

Set Shell "bash"
Set FontFamily "JetBrains Mono"
Set FontSize 20
Set Width 1200
Set Height 470
Set Padding 30
Set Theme { "name": "mitos", "background": "#04050A", "foreground": "#E6E9EF", "cursor": "#FF45C8", "selection": "#1B1F2A", "black": "#04050A", "red": "#FF45C8", "green": "#4DF0A0", "yellow": "#FFC24B", "blue": "#3DDCFF", "magenta": "#FF45C8", "cyan": "#3DDCFF", "white": "#E6E9EF", "brightBlack": "#5B6472", "brightRed": "#FF45C8", "brightGreen": "#4DF0A0", "brightYellow": "#FFC24B", "brightBlue": "#3DDCFF", "brightMagenta": "#FF45C8", "brightCyan": "#3DDCFF", "brightWhite": "#FFFFFF" }

Set TypingSpeed 55ms
Require python3

Hide
Type 'export PYTHONPATH="$MITOS_DEMO_PYTHONPATH"' Enter
Type "PS1='$ '" Enter
Type "PROMPT_COMMAND=" Enter
Type "clear" Enter
Show

Type "python3 -q" Sleep 400ms Enter
Sleep 800ms
Type "import mitos" Sleep 150ms Enter
Sleep 500ms
Type 'sb = mitos.create("python")' Sleep 150ms Enter
Sleep 1000ms
Type 'sb.exec("echo hello").stdout' Sleep 150ms Enter
Sleep 800ms
Type 'sb.run_code("import math; math.sqrt(144)").text' Sleep 150ms Enter
Sleep 800ms
Type "a, b = sb.fork(2)" Sleep 150ms Enter
Sleep 1000ms
Type 'a.exec("echo conservative > /tmp/p; cat /tmp/p").stdout' Sleep 150ms Enter
Sleep 800ms
Type 'b.exec("echo aggressive > /tmp/p; cat /tmp/p").stdout' Sleep 150ms Enter
Sleep 900ms
Type "sb.terminate()" Sleep 150ms Enter
Sleep 1100ms
Type "exit()" Enter
Sleep 700ms
