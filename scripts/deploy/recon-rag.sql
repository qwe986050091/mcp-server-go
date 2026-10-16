-- 只读侦察：知识库存量分布（供 S6 端到端验收挑选测试用户/查询）
select coalesce(metadata->>'userId', '?') as uid, count(*) as n
from rag_embeddings
where metadata->>'type' = 'document'
group by 1
order by 2 desc
limit 5;

select coalesce(metadata->>'source', '?') as source, count(*) as n
from rag_embeddings
where metadata->>'type' = 'document'
group by 1
order by 2 desc
limit 8;

select coalesce(metadata->>'type', '?') as type, count(*) as n
from rag_embeddings
group by 1
order by 2 desc;
