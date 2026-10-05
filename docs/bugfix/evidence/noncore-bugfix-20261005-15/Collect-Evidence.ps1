param([string]$RepoRoot='D:/whb_s/cube-core',[string]$Scratch='D:/whb_s/.tmp/noncore-review-20261005-25')
$ErrorActionPreference='Stop'
$runs=@('health-red','resume-red','health-green','resume-green','lifecycle-green','formal-green','overlay/overlay-red')
$summary=@()
foreach($name in $runs){
    $file=Join-Path $Scratch ($name+'.jsonl')
    $events=Get-Content -LiteralPath $file|ForEach-Object{try{$_|ConvertFrom-Json -ErrorAction Stop}catch{throw "Invalid JSON log: $file"}}
    $terminal=@($events|Where-Object{$_.Test -and $_.Action -in @('pass','fail','skip')})
    $leaves=@($terminal|Where-Object{ $current=$_; !($terminal|Where-Object{$_.Package -eq $current.Package -and $_.Test.StartsWith($current.Test+'/')}|Select-Object -First 1) })
    $summary+=[pscustomobject]@{run=$name;pass=@($leaves|Where-Object Action -eq 'pass').Count;fail=@($leaves|Where-Object Action -eq 'fail').Count;skip=@($leaves|Where-Object Action -eq 'skip').Count;tests=@($leaves|Select-Object Package,Test,Action)}
    if($name -match 'red'){
        $label=$name.Replace('/','-')
        $output=@($events|Where-Object Action -eq 'output'|ForEach-Object{$_.Output}) -join ''
        [IO.File]::WriteAllText((Join-Path $PSScriptRoot ($label+'.txt')),$output,[Text.UTF8Encoding]::new($false))
    }
}
$summary|ConvertTo-Json -Depth 6|Set-Content (Join-Path $PSScriptRoot 'formal-results.json')
$coverage=Get-Content (Join-Path $RepoRoot 'docs/review/evidence/noncore-review-20261005-25/coverage.json') -Raw|ConvertFrom-Json
$records=foreach($entry in $coverage.paths){
    $relative=$entry.path
    $content=[IO.File]::ReadAllText((Join-Path $RepoRoot $relative)).Replace("`r`n","`n")
    $digest=[Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($content))).ToLowerInvariant()
    [pscustomobject]@{path=$relative;sha256LF=$digest;coverage=$entry.status;freshness=$entry.freshness;source='current source; material ranges verified, not an exhaustive file audit'}
}
$records|Export-Csv -NoTypeInformation (Join-Path $RepoRoot 'docs/review/evidence/noncore-review-20261005-25/source-hashes.csv')
$summary|Select-Object run,pass,fail,skip|Format-Table
