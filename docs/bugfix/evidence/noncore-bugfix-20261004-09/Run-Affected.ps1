param([Parameter(Mandatory=$true)][string]$GoExecutable,[string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,[Parameter(Mandatory=$true)][string]$OutputRoot)
$ErrorActionPreference='Stop';$env:GOWORK='off'
if(Test-Path -LiteralPath $OutputRoot){throw 'Choose a new OutputRoot'}
New-Item -ItemType Directory -Path $OutputRoot|Out-Null
Push-Location $RepoRoot
try{
 $packages=@('./dataengine/engine','./kit/dataengine','./kit/service/...')
 & $GoExecutable test -race -count=1 -timeout=180s -json @packages 2>&1|Set-Content (Join-Path $OutputRoot 'tests.jsonl') -Encoding utf8NoBOM
 $testExit=$LASTEXITCODE
 & $GoExecutable vet @packages 2>&1|Set-Content (Join-Path $OutputRoot 'vet.log') -Encoding utf8NoBOM
 $vetExit=$LASTEXITCODE
 @{test_exit=$testExit;vet_exit=$vetExit}|ConvertTo-Json|Set-Content (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
 if($testExit -ne 0 -or $vetExit -ne 0){throw 'Affected consumer verification failed'}
}finally{Pop-Location}
