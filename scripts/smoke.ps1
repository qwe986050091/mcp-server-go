# MCP Server smoke test.
#  - stdio leg: feed fixed frames, assert stdout line count, per-line JSON validity
#    and key protocol fields.
#  - streamable http leg: start --transport http, drive it with the exact headers
#    LangChain4j's StreamableHttpMcpTransport sends, assert the status-code matrix.
#
# NOTE: this file is intentionally ASCII-only. Windows PowerShell 5.1 parses .ps1
# files using the ANSI code page unless a UTF-8 BOM is present, so non-ASCII text
# would break parsing whenever the BOM is lost by an editor/write tool.
#
# Go is not on PATH; default binary path is D:\program\gosdk\go1.27.1\bin\go.exe.
# Override with the GO_BIN environment variable.

# Fail fast on any unexpected PowerShell error. Native commands are never invoked
# through the PowerShell pipe (which would raise NativeCommandError for stderr
# output) -- both legs drive the child with System.Diagnostics / HttpClient instead.
$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $PSScriptRoot
$go = if ($env:GO_BIN) { $env:GO_BIN } else { "D:\program\gosdk\go1.27.1\bin\go.exe" }
$exe = Join-Path $root "bin\mcp-server-go.exe"

$failures = New-Object System.Collections.Generic.List[string]

function Assert-True {
    param([bool]$Condition, [string]$Message)
    if ($Condition) {
        Write-Host "  [OK]   $Message"
    } else {
        Write-Host "  [FAIL] $Message"
        $failures.Add($Message)
    }
}

Write-Host "== build =="
if (-not (Test-Path $go)) { throw "go executable not found: $go (set GO_BIN to override)" }
Push-Location $root
try {
    & $go build -o $exe ./cmd/mcp-server
    if ($LASTEXITCODE -ne 0) { throw "go build failed (exit code $LASTEXITCODE)" }
} finally {
    Pop-Location
}
Assert-True (Test-Path $exe) "built $exe"

# Frames are ASCII only: PowerShell 5.1 stdin encoding is ASCII by default.
$frames = @(
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"smoke","version":"1.0.0"}}}',
    '{"jsonrpc":"2.0","method":"notifications/initialized"}',
    '{"jsonrpc":"2.0","id":"p-1","method":"ping"}',
    '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}',
    '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"calc","arguments":{"expression":"1+2*3"}}}',
    '{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"calc","arguments":{"expression":"1/0"}}}',
    '{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"code_run","arguments":{"language":"python","code":"print(1+1)"}}}',
    '{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"kb_search","arguments":{"query":"smoke"}}}',
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}',
    '{"jsonrpc":"2.0","id":4,"method":"no/such/method"}',
    '{"this is not json'
)
$expectedResponses = 10

$payload = ($frames -join "`n") + "`n"
Write-Host "== smoke (stdio) =="

# Drive the child through System.Diagnostics so that
#   a) stdin carries exact BOM-less UTF-8 bytes (no $OutputEncoding surprises), and
#   b) the child's stderr log lines never surface as PowerShell NativeCommandError.
$psi = New-Object System.Diagnostics.ProcessStartInfo
$psi.FileName = $exe
$psi.Arguments = "--transport stdio"
$psi.UseShellExecute = $false
$psi.RedirectStandardInput = $true
$psi.RedirectStandardOutput = $true
$psi.RedirectStandardError = $true
$psi.StandardOutputEncoding = New-Object System.Text.UTF8Encoding($false)
$psi.StandardErrorEncoding = New-Object System.Text.UTF8Encoding($false)

$proc = [System.Diagnostics.Process]::Start($psi)
$outTask = $proc.StandardOutput.ReadToEndAsync()
$errTask = $proc.StandardError.ReadToEndAsync()
$stdinBytes = [System.Text.Encoding]::UTF8.GetBytes($payload)
$proc.StandardInput.BaseStream.Write($stdinBytes, 0, $stdinBytes.Length)
$proc.StandardInput.BaseStream.Flush()
$proc.StandardInput.Close()
$stdoutText = $outTask.GetAwaiter().GetResult()
$stderrText = $errTask.GetAwaiter().GetResult()
$proc.WaitForExit()
$code = $proc.ExitCode

$stdout = @($stdoutText -split "`n" | ForEach-Object { $_.TrimEnd("`r") } | Where-Object { $_ -ne "" })
$stderr = @($stderrText -split "`n" | ForEach-Object { $_.TrimEnd("`r") } | Where-Object { $_ -ne "" })

Assert-True ($code -eq 0) "exit code is 0 (actual $code)"
Assert-True ($stdout.Count -eq $expectedResponses) "stdout has exactly $expectedResponses response lines (actual $($stdout.Count))"
Assert-True (($stdout | Where-Object { $_ -match '\[mcp-server-go\]' }).Count -eq 0) "stdout carries no logs (logs go to stderr only)"
Assert-True ($stderr.Count -gt 0) "stderr has log output ($($stderr.Count) lines)"

# Per-line JSON validity plus key field assertions.
$jsonLines = New-Object System.Collections.Generic.List[object]
foreach ($line in $stdout) {
    try {
        $jsonLines.Add(($line | ConvertFrom-Json))
    } catch {
        Assert-True $false "line $($jsonLines.Count + 1) is not valid JSON: $line"
    }
}
Assert-True ($jsonLines.Count -eq $stdout.Count) "every stdout line is valid JSON"

$byId = @{}
foreach ($msg in $jsonLines) { $byId["$($msg.id)"] = $msg }

Assert-True ($byId['1'].result.protocolVersion -eq '2025-11-25') "initialize echoes protocolVersion 2025-11-25"
Assert-True ($byId['1'].result.serverInfo.name -eq 'mcp-server-go') "initialize returns serverInfo.name"
Assert-True ($null -ne $byId['1'].result.capabilities.tools) "initialize advertises tools capability"
Assert-True ($null -ne $byId['p-1'].result) "ping returns a result"

$toolNames = (($byId['2'].result.tools | ForEach-Object { $_.name }) -join ',')
Assert-True ($byId['2'].result.tools.Count -eq 5) "tools/list returns 5 tools (actual $($byId['2'].result.tools.Count))"
Assert-True ($toolNames -eq 'calc,web_fetch,web_search,code_run,kb_search') "tools/list preserves registration order (actual $toolNames)"
foreach ($tool in $byId['2'].result.tools) {
    Assert-True ($tool.inputSchema.type -eq 'object') "$($tool.name) inputSchema is an object schema"
    Assert-True ($tool.description.Length -gt 10) "$($tool.name) carries a description"
}
Assert-True ($null -eq $byId['2'].result.tools[4].inputSchema.properties.user_id) "kb_search does not expose user_id to the model"

Assert-True ($byId['5'].result.content[0].type -eq 'text') "tools/call result carries a text content block"
Assert-True ($byId['5'].result.content[0].text -eq '7') "calc 1+2*3 = 7 (actual '$($byId['5'].result.content[0].text)')"
Assert-True ($null -eq $byId['5'].result.isError) "calc success is not flagged as isError"
Assert-True ($byId['6'].result.isError -eq $true) "calc 1/0 is flagged as isError"
Assert-True ($byId['6'].result.content[0].text.Contains('1/0')) "calc 1/0 echoes the offending expression in the reason"
Assert-True ($byId['6'].result.content[0].text.Length -gt 5) "calc 1/0 returns a readable reason"

# code_run: with Docker it runs in the sandbox; without Docker it must fail loudly and
# must NEVER fall back to running the code on the host.
$codeRunText = $byId['7'].result.content[0].text
Assert-True ($byId['7'].result.content[0].type -eq 'text') "code_run returns a text content block"
Assert-True ($codeRunText.Length -gt 0) "code_run returns a readable message"
if ($byId['7'].result.isError -eq $true) {
    Assert-True ($codeRunText.Contains('docker')) "code_run without Docker explains the missing sandbox"
} else {
    Assert-True ($codeRunText.Contains('2')) "code_run inside Docker executed print(1+1)"
}

# kb_search: the knowledge base is optional, so an unavailable backend must degrade
# into a plain text answer instead of an isError result.
$kbText = $byId['8'].result.content[0].text
Assert-True ($kbText.Length -gt 0) "kb_search returns a readable message"
Assert-True ($null -eq $byId['8'].result.isError) "kb_search degrades without flagging isError"

Assert-True ($byId['3'].error.code -eq -32602) "unknown tool returns -32602"
Assert-True ($byId['4'].error.code -eq -32601) "unknown method returns -32601"

# A malformed frame must yield -32700 with id null.
$badFrame = $jsonLines | Where-Object { $_.error.code -eq -32700 }
Assert-True ($null -ne $badFrame) "malformed frame returns -32700 (parse error)"
if ($null -ne $badFrame) {
    Assert-True ($null -eq $badFrame.id) "parse-error response carries id null"
}

Write-Host ""
Write-Host "== smoke (streamable http) =="
Add-Type -AssemblyName System.Net.Http

$httpHost = "127.0.0.1"
$httpPort = 3001
$httpBase = "http://$httpHost`:$httpPort"
$httpOutFile = Join-Path $env:TEMP "mcp-server-go.smoke.http.out.txt"
$httpErrFile = Join-Path $env:TEMP "mcp-server-go.smoke.http.err.txt"

# Refuse to start if the port is already taken: otherwise the binary fails to bind
# and every following assertion fails for the wrong reason.
$busy = @(Get-NetTCPConnection -LocalPort $httpPort -State Listen -ErrorAction SilentlyContinue)
Assert-True ($busy.Count -eq 0) "port $httpPort is free before starting the http server"

$httpProc = $null
try {
    $httpProc = Start-Process -FilePath $exe -PassThru -NoNewWindow `
        -ArgumentList @("--transport", "http", "--addr", "$httpHost`:$httpPort", "--path", "/mcp") `
        -RedirectStandardOutput $httpOutFile -RedirectStandardError $httpErrFile

    $ready = $false
    $deadline = (Get-Date).AddSeconds(10)
    while ((Get-Date) -lt $deadline) {
        try {
            $probe = New-Object System.Net.Sockets.TcpClient
            $probe.Connect($httpHost, $httpPort)
            $probe.Close()
            $ready = $true
            break
        } catch {
            Start-Sleep -Milliseconds 150
        }
    }
    Assert-True $ready "http endpoint became reachable on $httpHost`:$httpPort"

    $client = New-Object System.Net.Http.HttpClient
    $client.Timeout = [System.TimeSpan]::FromSeconds(20)

    # Sends a request the same way LangChain4j's StreamableHttpMcpTransport does.
    function Invoke-Mcp {
        param([string]$Method, [string]$Path, [string]$Body, [hashtable]$Headers)

        $httpMethod = switch ($Method) {
            'POST' { [System.Net.Http.HttpMethod]::Post }
            'GET' { [System.Net.Http.HttpMethod]::Get }
            'DELETE' { [System.Net.Http.HttpMethod]::Delete }
        }
        $req = New-Object System.Net.Http.HttpRequestMessage($httpMethod, "$httpBase$Path")
        if (-not [string]::IsNullOrEmpty($Body)) {
            $req.Content = New-Object System.Net.Http.StringContent($Body, [System.Text.Encoding]::UTF8, "application/json")
        }
        foreach ($key in $Headers.Keys) {
            [void]$req.Headers.TryAddWithoutValidation($key, $Headers[$key])
        }
        $resp = $client.SendAsync($req).GetAwaiter().GetResult()
        $text = ""
        if ($null -ne $resp.Content) { $text = $resp.Content.ReadAsStringAsync().GetAwaiter().GetResult() }
        $sid = ""
        $ct = ""
        try { $sid = @($resp.Headers.GetValues("Mcp-Session-Id"))[0] } catch { $sid = "" }
        try { $ct = @($resp.Content.Headers.GetValues("Content-Type"))[0] } catch { $ct = "" }
        $result = New-Object psobject -Property @{
            Status = [int]$resp.StatusCode
            Sid = $sid
            ContentType = $ct
            Body = $text
        }
        $resp.Dispose()
        return $result
    }

    $clientHeaders = @{ "Accept" = "application/json,text/event-stream" }
    $initFrame = '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"smoke","version":"1.0.0"}}}'

    $init = Invoke-Mcp -Method POST -Path "/mcp" -Body $initFrame -Headers $clientHeaders
    Assert-True ($init.Status -eq 200) "POST initialize returns 200 (actual $($init.Status))"
    Assert-True ($init.ContentType -like "*application/json*") "initialize Content-Type is application/json (actual '$($init.ContentType)')"
    Assert-True ($init.Sid.Length -eq 32) "initialize assigns a 32-hex Mcp-Session-Id (actual '$($init.Sid)')"
    Assert-True ($init.Body -like '*"protocolVersion":"2025-11-25"*') "initialize echoes protocolVersion over http"
    Assert-True (($init.Body | ConvertFrom-Json).result.serverInfo.name -eq 'mcp-server-go') "initialize returns serverInfo over http"

    $noAccept = Invoke-Mcp -Method POST -Path "/mcp" -Body $initFrame -Headers @{ "Accept" = "application/json" }
    Assert-True ($noAccept.Status -eq 406) "POST without text/event-stream in Accept returns 406 (actual $($noAccept.Status))"

    $notif = Invoke-Mcp -Method POST -Path "/mcp" -Body '{"jsonrpc":"2.0","method":"notifications/initialized"}' -Headers $clientHeaders
    Assert-True ($notif.Status -eq 202) "notification returns 202 (actual $($notif.Status))"
    Assert-True ($notif.Body -eq "") "notification returns an empty body (actual '$($notif.Body)')"

    $noSession = Invoke-Mcp -Method POST -Path "/mcp" -Body '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' -Headers $clientHeaders
    Assert-True ($noSession.Status -eq 200) "request without a session id is accepted (actual $($noSession.Status))"

    $toolList = Invoke-Mcp -Method POST -Path "/mcp" -Body '{"jsonrpc":"2.0","id":3,"method":"tools/list"}' -Headers @{ "Accept" = "application/json,text/event-stream"; "Mcp-Session-Id" = $init.Sid }
    Assert-True ($toolList.Status -eq 200) "request with the assigned session id is accepted (actual $($toolList.Status))"
    $tools = $toolList.Body | ConvertFrom-Json
    Assert-True ($tools.result.tools.Count -eq 5) "tools/list over http returns 5 tools (actual $($tools.result.tools.Count))"

    $calc = Invoke-Mcp -Method POST -Path "/mcp" -Body '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"calc","arguments":{"expression":"sqrt(2)^2"}}}' -Headers @{ "Accept" = "application/json,text/event-stream"; "Mcp-Session-Id" = $init.Sid }
    $calcBody = $calc.Body | ConvertFrom-Json
    Assert-True ($calcBody.result.content[0].text -eq '2.0000000000000004') "tools/call over http evaluates an expression"

    $get = Invoke-Mcp -Method GET -Path "/mcp" -Body $null -Headers @{ "Accept" = "text/event-stream" }
    Assert-True ($get.Status -eq 405) "GET /mcp returns 405 (actual $($get.Status))"

    $del = Invoke-Mcp -Method DELETE -Path "/mcp" -Body $null -Headers @{ "Mcp-Session-Id" = $init.Sid }
    Assert-True ($del.Status -eq 204) "DELETE /mcp returns 204 (actual $($del.Status))"

    $slash = Invoke-Mcp -Method POST -Path "/mcp/" -Body $initFrame -Headers $clientHeaders
    Assert-True ($slash.Status -eq 404) "POST /mcp/ returns 404 with no redirect (actual $($slash.Status))"

    $evil = Invoke-Mcp -Method POST -Path "/mcp" -Body $initFrame -Headers @{ "Accept" = "application/json,text/event-stream"; "Origin" = "https://evil.example.com" }
    Assert-True ($evil.Status -eq 403) "non-loopback Origin is rejected with 403 (actual $($evil.Status))"

    $client.Dispose()
} finally {
    if ($null -ne $httpProc -and -not $httpProc.HasExited) {
        Stop-Process -Id $httpProc.Id -Force
        $httpProc.WaitForExit(5000) | Out-Null
    }
}

Start-Sleep -Milliseconds 300
$httpStdout = @(Get-Content -LiteralPath $httpOutFile -Encoding UTF8 -ErrorAction SilentlyContinue)
$httpStderr = @(Get-Content -LiteralPath $httpErrFile -Encoding UTF8 -ErrorAction SilentlyContinue)
Assert-True ($httpStdout.Count -eq 0) "http mode writes nothing to stdout (actual $($httpStdout.Count) lines)"
Assert-True ($httpStderr.Count -gt 0) "http mode logs to stderr ($($httpStderr.Count) lines)"

Write-Host ""
if ($failures.Count -gt 0) {
    Write-Host "SMOKE FAILED: $($failures.Count) assertion(s)"
    $failures | ForEach-Object { Write-Host "  - $_" }
    exit 1
}
Write-Host "SMOKE PASSED."
exit 0
