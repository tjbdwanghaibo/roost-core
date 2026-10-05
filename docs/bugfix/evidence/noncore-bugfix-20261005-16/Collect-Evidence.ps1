param([string]$Scratch='D:/whb_s/.tmp/noncore-review-20261005-26',[string]$RepoRoot='D:/whb_s/cube-core')
$ErrorActionPreference='Stop'
$runs=@('red','formal','native','matrix/race','matrix/root','overlay/overlay-red','codegen')
$summary=@()
foreach($name in $runs) {
    $file=Join-Path $Scratch ($name+'.jsonl')
    if(!(Test-Path -LiteralPath $file)){throw "Missing evidence $file"}
    $events=Get-Content -LiteralPath $file|ForEach-Object {try{$_|ConvertFrom-Json -ErrorAction Stop}catch{throw "Invalid JSON log: $file"}}
    $terminal=@($events|Where-Object{$_.Test -and $_.Action -in @('pass','fail','skip')})
    $leaves=@($terminal|Where-Object { $current=$_; !($terminal|Where-Object{$_.Package -eq $current.Package -and $_.Test.StartsWith($current.Test+'/')}|Select-Object -First 1) })
    $summary+=[pscustomobject]@{run=$name;pass=@($leaves|Where-Object Action -eq 'pass').Count;fail=@($leaves|Where-Object Action -eq 'fail').Count;skip=@($leaves|Where-Object Action -eq 'skip').Count;tests=@($leaves|Select-Object Package,Test,Action)}
    if($name -match 'red') {
        $text=@($events|Where-Object Action -eq 'output'|ForEach-Object{$_.Output}) -join ''
        [IO.File]::WriteAllText((Join-Path $PSScriptRoot ($name.Replace('/','-')+'.txt')),$text,[Text.UTF8Encoding]::new($false))
    }
}
$summary|ConvertTo-Json -Depth 6|Set-Content (Join-Path $PSScriptRoot 'results.json')
$coveragePath=Join-Path $RepoRoot 'docs/review/evidence/noncore-review-20261005-26/coverage.json'
$coverage=Get-Content -LiteralPath $coveragePath -Raw|ConvertFrom-Json
$records=foreach($entry in $coverage.paths) {
    $path=Join-Path $RepoRoot $entry.path
    if(!(Test-Path -LiteralPath $path)){continue}
    $raw=[IO.File]::ReadAllText($path).Replace("`r`n","`n")
    $digest=[Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($raw))).ToLowerInvariant()
    [pscustomobject]@{path=$entry.path;sha256LF=$digest;coverage=$entry.status;freshness=$entry.freshness}
}
$records|Export-Csv -NoTypeInformation (Join-Path (Split-Path $coveragePath) 'source-hashes.csv')
$summary|Select-Object run,pass,fail,skip|Format-Table
