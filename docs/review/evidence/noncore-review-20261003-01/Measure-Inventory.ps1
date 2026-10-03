param(
    [string]$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
    [string]$OutputRoot = $PSScriptRoot
)
$ErrorActionPreference = 'Stop'
$units = @{}
$groups = [ordered]@{
    'N01' = @('app','lifecycle','manager','health','admin')
    'N02' = @('httpclient','httpserver','security','gateway','webroute')
    'N03' = @('bus','nats','servicerpc','etcd')
    'N04' = @('redis','mongo','cache','migration')
    'N05' = @('remoteentity','ownerroute')
    'N06' = @('service','saga','servicemetrics')
    'N07' = @('configdata','attribute','event','errcode')
    'N08' = @('codegen')
    'N09' = @('skill')
    'N10' = @('ai','actionflow','featureflag','hotcode')
    'N11' = @('spatial','timer','clock','index')
    'N12' = @('metrics','log','failurelog','robot')
    'N13' = @('container','safemap','goroutine','misc','internal')
    'N14' = @('kit')
    'N15' = @('scripts','cmd')
}
foreach ($unit in $groups.Keys) { foreach ($dir in $groups[$unit]) { $units[$dir] = $unit } }
$shared = @('nest','sync','dataengine','entity','nestwal','versionstore','worker','lock','fctx','syncstream')
$sha = (& git -c "safe.directory=$($RepoRoot.Replace('\','/'))" -C $RepoRoot rev-parse HEAD).Trim()
$files = & git -c "safe.directory=$($RepoRoot.Replace('\','/'))" -C $RepoRoot ls-files -s -- '*.go' '*.tmpl'
if ($LASTEXITCODE -ne 0) { throw 'git ls-files failed' }
$rows = @(foreach ($line in $files) {
    if ($line -notmatch '^\d+ ([0-9a-f]+) 0\t(.+)$') { throw "Unsupported git index row: $line" }
    $blob = $Matches[1]; $path = $Matches[2]
    if ($path -match '(^|/)(testdata|tests)/' -or $path -match '^(docs|demo|examples)/' -or $path -match '_test\.go(\.tmpl)?$') { continue }
    $top = ($path -split '/')[0]
    $unit = if ($shared -contains $top) { 'CORE-BOUNDARY' } elseif ($units.ContainsKey($top)) { $units[$top] } else { 'UNASSIGNED' }
    [pscustomobject][ordered]@{ Path=$path; Blob=$blob; Kind=($(if ($path.EndsWith('.tmpl')) {'template'} else {'go'})); Top=$top; Unit=$unit; SourceHead=$sha }
})
New-Item -ItemType Directory -Force -Path $OutputRoot | Out-Null
$rows | Sort-Object Path | Export-Csv -LiteralPath (Join-Path $OutputRoot 'inventory.csv') -NoTypeInformation -Encoding utf8NoBOM
$summary = [ordered]@{ source_head=$sha; candidate_go=@($rows | Where-Object Kind -eq 'go').Count; candidate_templates=@($rows | Where-Object Kind -eq 'template').Count; unassigned=@($rows | Where-Object Unit -eq 'UNASSIGNED').Count; units=@($rows | Group-Object Unit | ForEach-Object { [ordered]@{unit=$_.Name; go=@($_.Group | Where-Object Kind -eq 'go').Count; templates=@($_.Group | Where-Object Kind -eq 'template').Count} }) }
$summary | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath (Join-Path $OutputRoot 'inventory-summary.json') -Encoding utf8NoBOM
$summary | ConvertTo-Json -Depth 6
