param(
 [string]$Workspace='D:/whb_s',
 [string]$OutputDirectory=''
)
$ErrorActionPreference='Stop'
if(-not $OutputDirectory){$OutputDirectory=Join-Path $PSScriptRoot '.'}
$repos=@{core='cube-core';kit='cube-kit';codegen='cube-codegen'}
$inventory=[Collections.Generic.List[object]]::new()
$heads=@{}
foreach($repo in @('core','kit','codegen')){
 $root=Join-Path $Workspace $repos[$repo]
 $heads[$repo]=(git -C $root rev-parse HEAD).Trim()
 if($LASTEXITCODE -ne 0){throw "git failed: $repo"}
 git -C $root diff --quiet HEAD -- '*.go' '*.go.tmpl'
 if($LASTEXITCODE -ne 0){throw "Tracked Go source differs from HEAD: $repo"}
 $paths=git -C $root ls-files '*.go' '*.go.tmpl'
 if($LASTEXITCODE -ne 0){throw "ls-files failed: $repo"}
 foreach($path in $paths){
  $text=[IO.File]::ReadAllText((Join-Path $root $path))
  $stratum='primary'
  if($path -match '(^|/)(testdata|tests|mocks|mongotest)(/|$)' -or $path -match '_test\.go(?:\.tmpl)?$'){$stratum='test_fixture'}
  elseif($path -match '(^|/)(examples|example|demo)(/|$)'){$stratum='example_demo'}
  elseif((($text -split "`n" | Select-Object -First 5) -join "`n") -match '(?m)^// Code generated[^\r\n]*DO NOT EDIT'){$stratum='generated'}
  $directory=($path -replace '/[^/]+$','');if($directory -eq $path){$directory='.'}
  $inventory.Add([pscustomobject]@{repo=$repo;path=$path;stratum=$stratum;directory=$directory;lines=($text -split "`n").Count;basename=[IO.Path]::GetFileName($path);head=$heads[$repo]})
 }
}
$docsRoot=Join-Path $Workspace 'cube-core/docs'
$documents=@(Get-ChildItem (Join-Path $docsRoot 'review') -File | Where-Object {$_.Name -match '^(REVIEW-|IMPLEMENTATION-).*\.md$'})
$documents+=@(Get-ChildItem (Join-Path $docsRoot 'bug') -Filter 'REVIEW-*.md' -File)
$lines=[Collections.Generic.List[object]]::new()
foreach($document in $documents){
 $num=0
 foreach($line in [IO.File]::ReadAllLines($document.FullName)){
  $num++
  # Expand literal path/{a.go,b.go}; do not infer directories or globs as coverage.
  $expanded=$line
  foreach($m in [regex]::Matches($line,'([A-Za-z0-9_./-]+/)\{([^}]+)\}')){
   $replacement=(($m.Groups[2].Value -split ',') | ForEach-Object {$m.Groups[1].Value+$_.Trim()}) -join ' '
   $expanded=$expanded.Replace($m.Value,$replacement)
  }
  $lines.Add([pscustomobject]@{doc=$document.FullName.Substring($docsRoot.Length+1).Replace('\','/');line=$num;text=$expanded})
 }
}
$basenameCounts=@{};$pathCounts=@{}
foreach($f in $inventory){$basenameCounts[$f.basename]++;$pathCounts[$f.path]++}
# Confirmed partial-source evidence seeds from the independently executed 09-18 review.
# Whole-file review and behavioral completeness are deliberately NOT inferred.
$seed=@{
 'core/entity/entity_base.go'='entity lifecycle and Sync lock wiring';
 'core/entity/entity_factory.go'='BuildEntity and initEntitySync';
 'core/entity/subject_sync.go'='public config, packer and captured LSN';
 'core/entitysync/subscription.go'='public gate and room-call boundaries';
 'core/room/room_broadcast.go'='construction, subscription, flush and close';
 'core/room/room_manager.go'='construction and room lifecycle';
 'core/room/room_transport_sink.go'='transport config and actual wire consumption';
 'codegen/internal/entity/gen.go'='sync config template and generated consumer';
 'codegen/internal/entity/parse.go'='sync marker parsing';
 'codegen/internal/entity/main.go'='CLI generation entry'
}
$evidence=[Collections.Generic.List[object]]::new()
$rows=[Collections.Generic.List[object]]::new()
foreach($f in $inventory){
 $qualified='(?:cube-|roost-)?'+[regex]::Escape($f.repo)+'/'+[regex]::Escape($f.path)
 $patterns=@($qualified)
 if($f.path.Contains('/') -and $pathCounts[$f.path] -eq 1){$patterns+=[regex]::Escape($f.path)}
 if($basenameCounts[$f.basename] -eq 1){$patterns+=[regex]::Escape($f.basename)}
 $pattern='(?<![A-Za-z0-9_])(?:'+($patterns -join '|')+')(?![A-Za-z0-9_.])'
 $rx=[regex]::new($pattern);$hits=@(foreach($candidate in $lines){if($rx.IsMatch($candidate.text)){$candidate}})
 foreach($hit in $hits){$evidence.Add([pscustomobject]@{repo=$f.repo;path=$f.path;document=$hit.doc;line=$hit.line;excerpt=$hit.text})}
 $key=$f.repo+'/'+$f.path
 $verified=$seed.ContainsKey($key)
 $reviewedSha=''
 $sourceUnchanged='unknown'
 if($verified){
  $reviewedSha=if($f.repo -eq 'core'){'6e09124aa3ea4036c43592be5b6893bb61cdfaf0'}else{'2e09c169a354b61edc357cb78665c5d18a1dc199'}
  git -C (Join-Path $Workspace $repos[$f.repo]) diff --quiet $reviewedSha $heads[$f.repo] -- $f.path
  if($LASTEXITCODE -gt 1){throw 'Cannot check evidence baseline'}
  $sourceUnchanged=($LASTEXITCODE -eq 0)
 }
 $rows.Add([pscustomobject]@{repo=$f.repo;path=$f.path;stratum=$f.stratum;directory=$f.directory;physical_lines=$f.lines;head=$f.head;document_reference=($hits.Count -gt 0);reference_count=$hits.Count;confirmed_partial_source=$verified;confirmed_review= $(if($verified){'review/REVIEW-2026-09-18.md'}else{''});verified_scope=$(if($verified){$seed[$key]}else{''});reviewed_sha=$reviewedSha;source_unchanged_since_review=$sourceUnchanged;whole_file_complete='unknown';behavior_complete='unknown'})
}
New-Item -ItemType Directory -Force $OutputDirectory | Out-Null
$rows | Export-Csv (Join-Path $OutputDirectory 'FILES.csv') -NoTypeInformation -Encoding utf8
$evidence | Export-Csv (Join-Path $OutputDirectory 'EVIDENCE.csv') -NoTypeInformation -Encoding utf8
$modules=@($rows | Where-Object stratum -eq 'primary' | Group-Object repo,directory | ForEach-Object {
 $g=$_.Group
 [pscustomobject]@{repo=$g[0].repo;directory=$g[0].directory;files=$g.Count;referenced=@($g|Where-Object document_reference).Count;confirmed_partial=@($g|Where-Object confirmed_partial_source).Count;unreferenced=@($g|Where-Object {-not $_.document_reference}).Count}
})
$modules | Export-Csv (Join-Path $OutputDirectory 'MODULES.csv') -NoTypeInformation -Encoding utf8
$result=@{heads=$heads;documents=$documents.Count;files=$rows.Count;strata=@();summary=@()}
foreach($repo in @('core','kit','codegen')){
 foreach($stratum in @('primary','generated','example_demo','test_fixture')){
  $g=@($rows|Where-Object {$_.repo -eq $repo -and $_.stratum -eq $stratum})
  $result.strata+=@{repo=$repo;stratum=$stratum;files=$g.Count;physical_lines=($g|Measure-Object physical_lines -Sum).Sum}
 }
 $g=@($rows|Where-Object {$_.repo -eq $repo -and $_.stratum -eq 'primary'})
 $h=@($g|Where-Object document_reference);$v=@($g|Where-Object confirmed_partial_source)
 $result.summary+=@{repo=$repo;files=$g.Count;referenced=$h.Count;reference_percent=[math]::Round(100*$h.Count/$g.Count,2);confirmed_partial=$v.Count;confirmed_partial_percent=[math]::Round(100*$v.Count/$g.Count,2);physical_lines=($g|Measure-Object physical_lines -Sum).Sum;referenced_file_lines=($h|Measure-Object physical_lines -Sum).Sum}
}
$result|ConvertTo-Json -Depth 6|Set-Content (Join-Path $OutputDirectory 'SUMMARY.json') -Encoding utf8
$result|ConvertTo-Json -Depth 6
