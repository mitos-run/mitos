# mitos SDK demo — render with: make demo  (needs vhs + ~/.mitos_api_key + live mitos.run)
Output docs/assets/demo.gif

Set Shell "bash"
Set FontFamily "JetBrains Mono"
Set FontSize 20
Set Width 1240
Set Height 720
Set Padding 28
Set Theme { "name": "mitos", "background": "#04050A", "foreground": "#E6E9EF", "cursor": "#FF45C8", "selection": "#1B1F2A", "black": "#04050A", "red": "#FF45C8", "green": "#4DF0A0", "yellow": "#FFC24B", "blue": "#3DDCFF", "magenta": "#FF45C8", "cyan": "#3DDCFF", "white": "#E6E9EF", "brightBlack": "#5B6472", "brightRed": "#FF45C8", "brightGreen": "#4DF0A0", "brightYellow": "#FFC24B", "brightBlue": "#3DDCFF", "brightMagenta": "#FF45C8", "brightCyan": "#3DDCFF", "brightWhite": "#FFFFFF" }

Set TypingSpeed 45ms
Require python3

Hide
Type "clear" Enter
Show

Type "python3" Sleep 300ms Enter
Sleep 900ms
Type "import mitos" Sleep 200ms Enter
Sleep 500ms
Type 'sb = mitos.create("python")' Sleep 200ms Enter
Sleep 1600ms
Type 'print(sb.exec("echo hello").stdout)' Sleep 200ms Enter
Sleep 900ms
Type "a, b = sb.fork(2)" Sleep 200ms Enter
Sleep 1600ms
Type 'a.exec("echo conservative > /workspace/plan.txt")' Sleep 200ms Enter
Sleep 700ms
Type 'b.exec("echo aggressive > /workspace/plan.txt")' Sleep 200ms Enter
Sleep 700ms
Type "sb.terminate()" Sleep 200ms Enter
Sleep 1200ms
Type "exit()" Enter
Sleep 800ms
