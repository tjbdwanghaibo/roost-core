param([Parameter(Mandatory=$true)][string]$Scratch,[string]$RepoRoot='D:/whb_s/cube-core')
$ErrorActionPreference='Stop'
$repo=$RepoRoot
$out=Join-Path $repo 'docs/bugfix/evidence/noncore-bugfix-20261005-17'
$summary=foreach($name in @('red','formal','race','root','repro/overlay-red','mail')) {
    $events=Get-Content -LiteralPath (Join-Path $Scratch ($name+'.jsonl'))|ForEach-Object {$_|ConvertFrom-Json -ErrorAction Stop}
    $terminal=@($events|Where-Object {$_.Test -and $_.Action -in @('pass','fail','skip')})
    $leaves=@($terminal|Where-Object { $current=$_; !($terminal|Where-Object {$_.Package -eq $current.Package -and $_.Test.StartsWith($current.Test+'/')}|Select-Object -First 1) })
    if($name -match 'red') {
        $raw=@($events|Where-Object Action -eq 'output'|ForEach-Object {$_.Output}) -join ''
        [IO.File]::WriteAllText((Join-Path $out ($name.Replace('/','-')+'.txt')),$raw,[Text.UTF8Encoding]::new($false))
    }
    [pscustomobject]@{run=$name;pass=@($leaves|Where-Object Action -eq 'pass').Count;fail=@($leaves|Where-Object Action -eq 'fail').Count;skip=@($leaves|Where-Object Action -eq 'skip').Count;tests=@($leaves|Select-Object Package,Test,Action)}
}
$summary|ConvertTo-Json -Depth 6|Set-Content (Join-Path $out 'results.json')
$coverage=Get-Content -Raw (Join-Path $repo 'docs/review/evidence/noncore-review-20261005-27/coverage.json')|ConvertFrom-Json
$hashes=foreach($entry in $coverage.paths) {
    $raw=[IO.File]::ReadAllText((Join-Path $repo $entry.path)).Replace("`r`n","`n")
    [pscustomobject]@{path=$entry.path;sha256LF=[Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($raw))).ToLowerInvariant();freshness=$entry.freshness}
}
$hashes|Export-Csv -NoTypeInformation (Join-Path $repo 'docs/review/evidence/noncore-review-20261005-27/source-hashes.csv')
$summary|Select-Object run,pass,fail,skip|Format-Table
